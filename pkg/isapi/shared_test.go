package isapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

func TestSharedViewsKeepRemainingMicrophone(t *testing.T) {
	var opens, closes atomic.Int32
	frames := make(chan []byte, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/channels"):
			channels(w)
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
				data := make([]byte, 160)
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				if _, err = io.ReadFull(rw, data); err != nil {
					return
				}
				frames <- data
			}
		}
	}))
	defer server.Close()
	raw := cameraURL(server, "#mix=1#gain=12")
	low, err := DialShared(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer low.Stop()
	high, err := DialShared(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer high.Stop()
	if low.(*sharedClient).camera != high.(*sharedClient).camera {
		t.Fatal("separate physical sessions")
	}
	media := low.GetMedias()[0]
	codec := media.Codecs[0]
	one, two := core.NewReceiver(media, codec), core.NewReceiver(media, codec)
	if err = low.(core.Consumer).AddTrack(media, codec, one); err != nil {
		t.Fatal(err)
	}
	if err = high.(core.Consumer).AddTrack(media, codec, two); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 2)
	go func() { low.Start(); done <- struct{}{} }()
	go func() { high.Start(); done <- struct{}{} }()
	send := func(source *core.Receiver, seq uint16) {
		source.WriteRTP(&rtp.Packet{Header: rtp.Header{SSRC: 1, SequenceNumber: seq, Timestamp: uint32(seq) * 160}, Payload: tone(core.CodecPCMU, 1000, 10)})
	}
	send(one, 1)
	send(two, 1)
	select {
	case <-frames:
	case <-time.After(time.Second):
		t.Fatal("no mixed frame")
	}
	low.Stop()
	send(two, 2)
	select {
	case <-frames:
	case <-time.After(time.Second):
		t.Fatal("closing low stopped high microphone")
	}
	if opens.Load() != 1 || closes.Load() != 0 {
		t.Fatal("session disrupted by departing view")
	}
	high.Stop()
	<-done
	<-done
	if closes.Load() != 1 {
		t.Fatal("last view did not close camera")
	}
	sharedCameras.Lock()
	count := len(sharedCameras.cameras)
	sharedCameras.Unlock()
	if count != 0 {
		t.Fatal("registry leak")
	}
}
func TestSharedFailureCanReconnectBeforeOldViewStops(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/channels") {
			channels(w)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	raw := cameraURL(server, "#mix=1")
	first, err := DialShared(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Stop()
	if first.Start() == nil {
		t.Fatal("expected open error")
	}
	next, err := DialShared(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Stop()
	if first.(*sharedClient).camera == next.(*sharedClient).camera {
		t.Fatal("failed session reused forever")
	}
	first.Stop()
	sharedCameras.Lock()
	present := sharedCameras.cameras[next.(*sharedClient).key] == next.(*sharedClient).camera
	sharedCameras.Unlock()
	if !present {
		t.Fatal("old cleanup removed replacement")
	}
}
