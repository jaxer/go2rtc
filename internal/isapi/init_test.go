package isapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
)

// The registered handler must hand streams the producer itself: streams asserts
// core.Consumer on it for backchannel tracks and panicked on a wrapper (7a19b93).
func TestHandlerReturnsAConsumer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<TwoWayAudioChannel><id>1</id><audioCompressionType>G.711ulaw</audioCompressionType></TwoWayAudioChannel>`)
	}))
	defer server.Close()
	Init()
	prod, err := streams.GetProducer(strings.Replace(server.URL, "http://", "isapi://", 1) + "#mix=1#gain=24")
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Stop()
	if _, ok := prod.(core.Consumer); !ok {
		t.Fatalf("%T is not a core.Consumer", prod)
	}
}
