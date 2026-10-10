package streams

import (
	"sync"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

type talkbackTestProducer struct {
	core.Connection
	done chan struct{}
	once sync.Once
}

func newTalkbackTestProducer(media *core.Media) *talkbackTestProducer {
	return &talkbackTestProducer{
		Connection: core.Connection{Medias: []*core.Media{media}},
		done:       make(chan struct{}),
	}
}

func (p *talkbackTestProducer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, codec)
	sender.Handler = func(*core.Packet) {}
	sender.HandleRTP(track)
	p.Senders = append(p.Senders, sender)
	return nil
}

func (p *talkbackTestProducer) Start() error {
	<-p.done
	return nil
}

func (p *talkbackTestProducer) Stop() error {
	p.once.Do(func() { close(p.done) })
	return p.Connection.Stop()
}

// A browser can disconnect while a camera backchannel is reconnecting. The
// saved microphone receiver must not be reattached after the browser closes it.
func TestReconnectDoesNotReviveClosedTalkback(t *testing.T) {
	codec := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}}
	mic := core.NewReceiver(media, codec)
	old := newTalkbackTestProducer(media)
	require.NoError(t, old.AddTrack(media, codec, mic))

	next := newTalkbackTestProducer(media)
	const scheme = "closed-talkback-test"
	HandleFunc(scheme, func(string) (core.Producer, error) { return next, nil })
	t.Cleanup(func() { delete(handlers, scheme) })
	p := &Producer{url: scheme + ":", conn: old, senders: []*core.Receiver{mic}, state: stateStart, workerID: 1}
	t.Cleanup(p.stop)

	mic.Close()
	require.Empty(t, mic.Senders())
	p.reconnect(1, 0)
	require.Empty(t, mic.Senders(), "reconnect resurrected the disconnected browser's microphone")
	require.Equal(t, stateNone, p.state)
	require.Nil(t, p.conn)
}

func TestReconnectTalkbackClosedDuringDial(t *testing.T) {
	codec := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}}
	mic := core.NewReceiver(media, codec)
	old := newTalkbackTestProducer(media)
	require.NoError(t, old.AddTrack(media, codec, mic))
	next := newTalkbackTestProducer(media)
	const scheme = "closing-talkback-test"
	HandleFunc(scheme, func(string) (core.Producer, error) {
		mic.Close()
		return next, nil
	})
	t.Cleanup(func() { delete(handlers, scheme) })
	p := &Producer{url: scheme + ":", conn: old, senders: []*core.Receiver{mic}, state: stateStart, workerID: 1}
	t.Cleanup(p.stop)
	p.reconnect(1, 0)
	require.Empty(t, mic.Senders())
	require.Equal(t, stateNone, p.state)
	select {
	case <-next.done:
	default:
		t.Fatal("new connection was not released after the microphone disconnected")
	}
}

func TestReconnectKeepsLiveTalkback(t *testing.T) {
	codec := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}}
	mic := core.NewReceiver(media, codec)
	old := newTalkbackTestProducer(media)
	require.NoError(t, old.AddTrack(media, codec, mic))
	next := newTalkbackTestProducer(media)
	const scheme = "live-talkback-test"
	HandleFunc(scheme, func(string) (core.Producer, error) { return next, nil })
	t.Cleanup(func() { delete(handlers, scheme) })
	p := &Producer{url: scheme + ":", conn: old, senders: []*core.Receiver{mic}, state: stateStart, workerID: 1}
	t.Cleanup(p.stop)
	p.reconnect(1, 0)
	require.Same(t, next, p.conn)
	require.NotEmpty(t, mic.Senders())
	require.False(t, mic.Closed())
	require.Len(t, next.Senders, 1)
	require.Equal(t, "connected", next.Senders[0].State())
}

func TestClosedMicrophoneRejectsNewSender(t *testing.T) {
	codec := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}}
	mic := core.NewReceiver(media, codec)
	mic.Close()
	sender := core.NewSender(media, codec)
	sender.Handler = func(*core.Packet) {}
	sender.HandleRTP(mic)
	require.Empty(t, mic.Senders())
	require.Equal(t, "closed", sender.State())
}

func TestReconnectKeepsIncomingMediaAfterMicrophoneCloses(t *testing.T) {
	codec := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
	talk := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}}
	listen := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}}
	mic := core.NewReceiver(talk, codec)
	old := newTalkbackTestProducer(talk)
	old.Medias = append(old.Medias, listen)
	require.NoError(t, old.AddTrack(talk, codec, mic))
	receiver, err := old.GetTrack(listen, codec)
	require.NoError(t, err)
	viewer := core.NewSender(listen, codec)
	viewer.Handler = func(*core.Packet) {}
	viewer.HandleRTP(receiver)
	t.Cleanup(viewer.Close)

	next := newTalkbackTestProducer(talk)
	next.Medias = append(next.Medias, listen)
	const scheme = "listen-after-talkback-test"
	HandleFunc(scheme, func(string) (core.Producer, error) { return next, nil })
	t.Cleanup(func() { delete(handlers, scheme) })
	p := &Producer{url: scheme + ":", conn: old, senders: []*core.Receiver{mic}, receivers: []*core.Receiver{receiver}, state: stateStart, workerID: 1}
	t.Cleanup(p.stop)
	mic.Close()
	p.reconnect(1, 0)
	require.Same(t, next, p.conn)
	require.Empty(t, next.Senders)
	require.Empty(t, p.senders)
	require.Len(t, p.receivers, 1)
	require.NotEmpty(t, p.receivers[0].Senders(), "camera listener must stay connected")
}
