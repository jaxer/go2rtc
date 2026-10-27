package isapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/pion/rtp"
)

func cameraURL(server *httptest.Server, options string) string {
	return strings.Replace(server.URL, "http://", "isapi://", 1) + options
}
func channels(w http.ResponseWriter) {
	io.WriteString(w, `<TwoWayAudioChannel><id>1</id><audioCompressionType>G.711ulaw</audioCompressionType></TwoWayAudioChannel>`)
}
func TestDialMixerOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { channels(w) }))
	defer server.Close()
	for _, opt := range []string{"#gain=24", "#mix=1#gain=25", "#mix=1#gain=NaN", "#mix=1#gain=-1",
		"#target=-12", "#mix=1#target=-12", "#mix=1#gain=24#target=NaN",
		"#compress=moderate", "#mix=1#compress=unknown",
		"#mix=1#gain=24#target=-25", "#mix=1#gain=24#target=-5"} {
		if c, err := Dial(cameraURL(server, opt)); err == nil {
			c.Stop()
			t.Fatalf("accepted %s", opt)
		}
	}
	c, err := Dial(cameraURL(server, "#mix=1#gain=24"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if c.mixer == nil || c.mixer.maxGain < 15 {
		t.Fatal("mix options ignored")
	}
	if c.mixer.compression != nil {
		t.Fatal("compression must default off")
	}
	if c.mixer.targetRMS != 4096 {
		t.Fatal("default target changed")
	}
	compressed, err := Dial(cameraURL(server, "#mix=1#gain=24#target=-6#compress=moderate"))
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Stop()
	if compressed.mixer.compression == nil {
		t.Fatal("compression ignored")
	}
	louder, err := Dial(cameraURL(server, "#mix=1#gain=24#target=-12"))
	if err != nil {
		t.Fatal(err)
	}
	defer louder.Stop()
	if louder.mixer.targetRMS < 8200 || louder.mixer.targetRMS > 8240 {
		t.Fatal("speech target ignored")
	}
}
func TestMixedCameraSession(t *testing.T) {
	var opens, closes atomic.Int32
	audio := make(chan []byte, 1)
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
		case strings.HasSuffix(r.URL.Path, "/audioData"):
			// Real Hikvision audioData is HTTP headers followed by raw outbound G.711.
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
			rw.Flush()
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 160)
			if _, err = io.ReadFull(rw, buf); err != nil {
				t.Error(err)
				return
			}
			audio <- buf
			io.Copy(io.Discard, rw)
		}
	}))
	defer server.Close()
	c, err := Dial(cameraURL(server, "#mix=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	media := c.medias[0]
	codec := media.Codecs[0]
	sources := []*core.Receiver{core.NewReceiver(media, codec), core.NewReceiver(media, codec)}
	for _, source := range sources {
		if err = c.AddTrack(media, codec, source); err != nil {
			t.Fatal(err)
		}
		source.WriteRTP(&rtp.Packet{Header: rtp.Header{SSRC: 1, SequenceNumber: 1}, Payload: tone(core.CodecPCMU, 1000, 10)})
	}
	done := make(chan error, 1)
	go func() { done <- c.Start() }()
	select {
	case data := <-audio:
		if level(data) < 1200 {
			t.Fatal("both voices not present")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no audio")
	}
	if opens.Load() != 1 || closes.Load() != 0 {
		t.Fatal("mixed start interferes with existing session")
	}
	c.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not unblock Start")
	}
	if closes.Load() != 1 {
		t.Fatal("owned camera session not closed")
	}
}
func TestBusyCameraNotClosed(t *testing.T) {
	var closes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/channels") {
			channels(w)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/close") {
			closes.Add(1)
		}
		io.WriteString(w, `<statusCode>4</statusCode>`)
	}))
	defer server.Close()
	c, err := Dial(cameraURL(server, "#mix=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if c.Start() == nil {
		t.Fatal("busy response ignored")
	}
	if closes.Load() != 0 {
		t.Fatal("foreign session closed")
	}
}
func TestAudioDigestAndCancellation(t *testing.T) {
	authenticated := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if header == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="camera", nonce="1234", qop="auth", algorithm=MD5`)
			w.WriteHeader(401)
			return
		}
		cnonce := tcp.Between(header, `cnonce="`, `"`)
		expected := tcp.HexMD5(tcp.HexMD5("user", "camera", "pass"), "1234", "00000001", cnonce, "auth", tcp.HexMD5("PUT", r.URL.RequestURI()))
		if tcp.Between(header, `response="`, `"`) != expected {
			t.Error("incorrect Digest")
			w.WriteHeader(401)
			return
		}
		authenticated <- struct{}{}
		<-r.Context().Done() // cancel while waiting for response headers
	}))
	defer server.Close()
	c := &Client{url: strings.Replace(server.URL, "http://", "http://user:pass@", 1), channel: "1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := c.openAudio(ctx); done <- err }()
	select {
	case <-authenticated:
	case <-time.After(time.Second):
		t.Fatal("not authenticated")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake not canceled")
	}
}
func TestStaleSessionReclaim(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		code         int
		reclaim      bool
	}{
		{"camera", "", http.StatusNotFound, true},
		{"idle intercom", "idle", http.StatusOK, true},
		{"ringing intercom", "ring", http.StatusOK, false},
		{"on call", "onCall", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var held atomic.Bool
			var closes, opens atomic.Int32
			held.Store(true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/channels"):
					channels(w)
				case strings.HasSuffix(r.URL.Path, "/callStatus"):
					w.WriteHeader(tc.code)
					io.WriteString(w, `{"CallStatus":{"status":"`+tc.status+`"}}`)
				case strings.HasSuffix(r.URL.Path, "/close"):
					closes.Add(1)
					held.Store(false)
					io.WriteString(w, `<statusCode>1</statusCode>`)
				case strings.HasSuffix(r.URL.Path, "/open"):
					opens.Add(1)
					if held.Load() {
						w.WriteHeader(http.StatusForbidden)
						io.WriteString(w, `<statusCode>4</statusCode>`)
						return
					}
					io.WriteString(w, `<statusCode>1</statusCode>`)
				}
			}))
			defer server.Close()
			c, err := Dial(cameraURL(server, "#mix=1"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Stop()
			err = c.claim()
			if tc.reclaim != (err == nil) || tc.reclaim != (closes.Load() == 1) {
				t.Fatalf("reclaim=%v err=%v closes=%d", tc.reclaim, err, closes.Load())
			}
			if !tc.reclaim && !errors.Is(err, errInvalidOperation) {
				t.Fatalf("busy not reported: %v", err)
			}
			if tc.reclaim && opens.Load() != 2 {
				t.Fatalf("opens=%d", opens.Load())
			}
		})
	}
}
func TestRefusedOpenBacksOff(t *testing.T) {
	var opens atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/channels"):
			channels(w)
		case strings.HasSuffix(r.URL.Path, "/open"):
			opens.Add(1)
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<statusCode>4</statusCode>`)
		default:
			io.WriteString(w, `{"CallStatus":{"status":"onCall"}}`)
		}
	}))
	defer server.Close()
	c, err := Dial(cameraURL(server, "#mix=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	start := time.Now()
	_ = c.claim()
	_ = c.claim()
	_ = c.claim()
	if elapsed := time.Since(start); elapsed < 3*time.Second || opens.Load() != 3 {
		t.Fatalf("no backoff: %v opens=%d", elapsed, opens.Load())
	}
	c.Stop()
	begin := time.Now()
	if err := c.claim(); err != context.Canceled || time.Since(begin) > time.Second {
		t.Fatalf("stopped client still waits: %v", err)
	}
}
func TestCloseSessions(t *testing.T) {
	var closes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/channels"):
			channels(w)
		case strings.HasSuffix(r.URL.Path, "/close"):
			closes.Add(1)
			io.WriteString(w, `<statusCode>1</statusCode>`)
		case strings.HasSuffix(r.URL.Path, "/audioData"):
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			io.WriteString(w, `<statusCode>1</statusCode>`)
		}
	}))
	defer server.Close()
	c, err := Dial(cameraURL(server, "#mix=1"))
	if err != nil {
		t.Fatal(err)
	}
	go c.Start()
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := 0
		openSessions.Range(func(any, any) bool { n++; return true })
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	CloseSessions()
	if closes.Load() != 1 {
		t.Fatalf("closes=%d", closes.Load())
	}
	c.Stop()
}

func TestDialUsesCachedChannelWhenTheCameraStalls(t *testing.T) {
	var probes atomic.Int32
	stalled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probes.Add(1) > 1 {
			<-stalled // the door station's web server stops answering
			return
		}
		channels(w)
	}))
	defer server.Close()
	defer close(stalled)
	first, err := Dial(cameraURL(server, "#mix=1#gain=24"))
	if err != nil {
		t.Fatal(err)
	}
	first.Stop()
	begin := time.Now()
	second, err := Dial(cameraURL(server, "#mix=1#gain=24"))
	if err != nil || time.Since(begin) > 500*time.Millisecond {
		t.Fatalf("second dial err=%v after %v: expected the cached channel", err, time.Since(begin))
	}
	if second.channel != "1" || second.medias[0].Codecs[0].Name != core.CodecPCMU {
		t.Fatalf("cached channel %q codec %q", second.channel, second.medias[0].Codecs[0].Name)
	}
	second.Stop()
	if probes.Load() != 1 {
		t.Fatalf("probes=%d, want 1", probes.Load())
	}
}
