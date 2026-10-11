package rtsp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/isapi"
	wire "github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/pion/rtp"
)

// Exercise the production RTSP handler, stream selection, mixer and raw ISAPI
// output together. All endpoints and generated samples stay inside this test.
func TestExistingStreamMicrophoneOnly(t *testing.T) {
	streams.HandleFunc("isapi", isapi.DialShared)
	defaultMedias = core.ParseQuery(map[string][]string{"video": {""}, "audio": {""}})
	for _, sameView := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_view_%t", sameView), func(t *testing.T) {
			var opens, closes, playbackDials atomic.Int32
			frames := make(chan []byte, 128)
			camera := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/channels"):
					io.WriteString(w, `<TwoWayAudioChannel><id>1</id><audioCompressionType>G.711ulaw</audioCompressionType></TwoWayAudioChannel>`)
				case strings.HasSuffix(r.URL.Path, "/open"):
					opens.Add(1)
					io.WriteString(w, `<statusCode>1</statusCode>`)
				case strings.HasSuffix(r.URL.Path, "/close"):
					closes.Add(1)
					io.WriteString(w, `<statusCode>1</statusCode>`)
				default:
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
					rw.Flush()
					for {
						conn.SetReadDeadline(time.Now().Add(3 * time.Second))
						frame := make([]byte, 160)
						if _, err = io.ReadFull(rw, frame); err != nil {
							return
						}
						select {
						case frames <- frame:
						default:
						}
					}
				}
			}))
			defer camera.Close()
			streams.HandleFunc("unneeded-playback", func(string) (core.Producer, error) {
				playbackDials.Add(1)
				return nil, errors.New("playback must not be contacted")
			})
			source := strings.Replace(camera.URL, "http://", "isapi://", 1) + "#mix=1#gain=24"
			for _, name := range []string{"gate_low", "gate_high"} {
				if _, err := streams.New(name, source, "unneeded-playback:camera"); err != nil {
					t.Fatal(err)
				}
				defer streams.Delete(name)
			}
			one := openTestMicrophone(t, "gate_low?backchannel=only")
			twoName := "gate_high"
			if sameView {
				twoName = "gate_low"
			}
			two := openTestMicrophone(t, twoName+"?backchannel=only")
			one.send(t, 1)
			two.send(t, 1)
			awaitSound(t, frames)
			if opens.Load() != 1 || closes.Load() != 0 {
				t.Fatal("clients did not share one camera session")
			}
			one.close(t)
			if closes.Load() != 0 {
				t.Fatal("departing microphone closed the camera")
			}
			// Drain previously mixed samples before proving that the remaining mic works.
			time.Sleep(150 * time.Millisecond)
			for len(frames) > 0 {
				<-frames
			}
			two.send(t, 2)
			awaitSound(t, frames)
			two.close(t)
			if closes.Load() != 1 {
				t.Fatalf("last departure: closes=%d", closes.Load())
			}
			if playbackDials.Load() != 0 {
				t.Fatalf("unnecessary playback connections: %d", playbackDials.Load())
			}
			// A new hold must open a fresh session after both previous clients leave.
			three := openTestMicrophone(t, "gate_low?backchannel=only")
			three.send(t, 1)
			awaitSound(t, frames)
			three.close(t)
			if opens.Load() != 2 || closes.Load() != 2 {
				t.Fatal("next hold did not recover cleanly")
			}
		})
	}
}

type testMicrophone struct {
	net.Conn
	done   chan struct{}
	closed bool
}

func openTestMicrophone(t *testing.T, path string) *testMicrophone {
	t.Helper()
	client, peer := net.Pipe()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	m := &testMicrophone{Conn: client, done: make(chan struct{})}
	go func() { defer close(m.done); tcpHandler(wire.NewServer(peer)) }()
	t.Cleanup(func() { m.close(t) })
	reader := bufio.NewReader(client)
	request := func(method, uri, headers string) *tcp.Response {
		t.Helper()
		if _, err := fmt.Fprintf(client, "%s rtsp://localhost/%s RTSP/1.0\r\nCSeq: 1\r\n%s\r\n", method, uri, headers); err != nil {
			t.Fatal(err)
		}
		res, err := tcp.ReadResponse(reader)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d", method, res.StatusCode)
		}
		return res
	}
	sdp := string(request("DESCRIBE", path, "").Body)
	if strings.Count(sdp, "m=audio") != 1 || strings.Contains(sdp, "m=video") || strings.Contains(sdp, "a=recvonly") || !strings.Contains(sdp, "a=sendonly") {
		t.Fatalf("unexpected playback SDP: %s", sdp)
	}
	res := request("SETUP", strings.Split(path, "?")[0]+"/trackID=0", "Transport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n")
	request("PLAY", path, "Session: "+res.Header.Get("Session")+"\r\n")
	return m
}
func (m *testMicrophone) send(t *testing.T, seq uint16) {
	t.Helper()
	samples := make([]byte, 160)
	for i := range samples {
		if i%16 < 8 {
			samples[i] = 0x9c
		} else {
			samples[i] = 0x1c
		}
	}
	p := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 1, SequenceNumber: seq, Timestamp: uint32(seq) * 160}, Payload: samples}
	data, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	frame := append([]byte{'$', 0, byte(len(data) >> 8), byte(len(data))}, data...)
	if _, err = m.Write(frame); err != nil {
		t.Fatal(err)
	}
}
func (m *testMicrophone) close(t *testing.T) {
	t.Helper()
	if m.closed {
		return
	}
	m.closed = true
	m.Conn.Close()
	select {
	case <-m.done:
	case <-time.After(3 * time.Second):
		t.Error("RTSP consumer did not clean up")
	}
}
func awaitSound(t *testing.T, frames <-chan []byte) {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case frame := <-frames:
			for _, sample := range frame {
				if sample != 0xff && sample != 0x7f {
					return
				}
			}
		case <-timeout.C:
			t.Fatal("no microphone audio reached the camera")
		}
	}
}

// Existing playback defaults and backchannel=1 remain compatible. Use a local
// duplex producer so the negotiated tracks, rather than a parser helper, decide.
func TestBackchannelPlaybackCompatibility(t *testing.T) {
	streams.HandleFunc("test-duplex", func(string) (core.Producer, error) {
		codec := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
		return &duplexTestProducer{Connection: core.Connection{Medias: []*core.Media{
			{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}},
			{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}},
		}}, done: make(chan struct{})}, nil
	})
	defaultMedias = core.ParseQuery(map[string][]string{"video": {""}, "audio": {""}})
	for _, tc := range []struct {
		query          string
		playback, talk bool
	}{
		{"", true, false},
		{"?audio=pcmu&backchannel=1", true, true},
		{"?backchannel=1", true, true},
		{"?backchannel=only", false, true},
		{"?audio=pcmu&backchannel=only", false, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if _, err := streams.New("compat", "test-duplex:camera"); err != nil {
				t.Fatal(err)
			}
			defer streams.Delete("compat")
			client, peer := net.Pipe()
			client.SetDeadline(time.Now().Add(2 * time.Second))
			done := make(chan struct{})
			go func() { defer close(done); tcpHandler(wire.NewServer(peer)) }()
			defer func() {
				client.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("cleanup timeout")
				}
			}()
			fmt.Fprintf(client, "DESCRIBE rtsp://localhost/compat%s RTSP/1.0\r\nCSeq: 1\r\n\r\n", tc.query)
			res, err := tcp.ReadResponse(bufio.NewReader(client))
			if err != nil {
				t.Fatal(err)
			}
			sdp := string(res.Body)
			if res.StatusCode != 200 || strings.Contains(sdp, "a=recvonly") != tc.playback || strings.Contains(sdp, "a=sendonly") != tc.talk {
				t.Fatalf("unexpected tracks: %d %s", res.StatusCode, sdp)
			}
		})
	}
}

type duplexTestProducer struct {
	core.Connection
	done chan struct{}
}

func (p *duplexTestProducer) Start() error { <-p.done; return nil }
func (p *duplexTestProducer) Stop() error {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return p.Connection.Stop()
}
func (p *duplexTestProducer) AddTrack(m *core.Media, c *core.Codec, r *core.Receiver) error {
	s := core.NewSender(m, c)
	s.Handler = func(*core.Packet) {}
	s.HandleRTP(r)
	p.Senders = append(p.Senders, s)
	return nil
}
