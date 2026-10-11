package rtsp

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
)

func TestServerBackchannelWithoutPlaybackTrack(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	client.SetDeadline(time.Now().Add(time.Second))
	server := NewServer(peer)
	server.Listen(func(event any) {
		if event == MethodDescribe {
			media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecPCMU, ClockRate: 8000}}}
			_, _ = server.GetTrack(media, media.Codecs[0])
		}
	})
	done := make(chan error, 1)
	go func() { done <- server.Accept() }()
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
		return res
	}
	res := request("DESCRIBE", "gate_talk?backchannel=1", "")
	if res.StatusCode != 200 {
		t.Fatalf("microphone-only DESCRIBE rejected: %d", res.StatusCode)
	}
	if !strings.Contains(string(res.Body), "a=sendonly") || strings.Contains(string(res.Body), "a=recvonly") {
		t.Fatalf("unexpected SDP: %s", res.Body)
	}
	res = request("SETUP", "gate_talk/trackID=0", "Transport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n")
	if res.StatusCode != 200 || res.Header.Get("Transport") != "RTP/AVP/TCP;unicast;interleaved=0-1" {
		t.Fatal("backchannel SETUP rejected")
	}
	res = request("PLAY", "gate_talk", "Session: "+res.Header.Get("Session")+"\r\n")
	if res.StatusCode != 200 {
		t.Fatal("backchannel PLAY rejected")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(server.Senders) != 0 || len(server.Receivers) != 1 {
		t.Fatal("unexpected playback dependency")
	}
	server.Stop()
}
