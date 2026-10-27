package isapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// streams adds backchannel tracks through core.Consumer; a producer that is not
// one makes the stream worker panic (7a19b93, 2026-10-27).
func TestSharedProducerIsAConsumer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { channels(w) }))
	defer server.Close()
	prod, err := DialShared(cameraURL(server, "#mix=1#gain=24"))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Stop()
	if _, ok := prod.(core.Consumer); !ok {
		t.Fatalf("%T is not a core.Consumer", prod)
	}
	plain, err := DialShared(cameraURL(server, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Stop()
	if _, ok := plain.(core.Consumer); !ok {
		t.Fatalf("%T is not a core.Consumer", plain)
	}
}
