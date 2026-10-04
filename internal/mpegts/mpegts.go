package mpegts

import (
	"net/http"
	"strings"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
)

func Init() {
	api.HandleFunc("api/stream.ts", apiHandle)
	api.HandleFunc("api/stream.aac", apiStreamAAC)
}

func apiHandle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		outputMpegTS(w, r)
	} else {
		inputMpegTS(w, r)
	}
}

func outputMpegTS(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")
	stream := streams.Get(src)
	if stream == nil {
		http.Error(w, api.StreamNotFound, http.StatusNotFound)
		return
	}

	cons := mpegts.NewConsumer()
	cons.WithRequest(r)
	// ?audio=pcmu,pcma adds G.711 next to AAC (any other value is ignored)
	if values := r.URL.Query()["audio"]; len(values) > 0 {
		var pcmu, pcma bool
		for _, v := range values {
			for _, name := range strings.Split(strings.ToLower(v), ",") {
				pcmu = pcmu || name == "pcmu"
				pcma = pcma || name == "pcma"
			}
		}
		cons.WithG711(pcmu, pcma)
	}

	if err := stream.AddConsumer(cons); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Add("Content-Type", "video/mp2t")

	_, _ = cons.WriteTo(w)

	stream.RemoveConsumer(cons)
}

func inputMpegTS(w http.ResponseWriter, r *http.Request) {
	dst := r.URL.Query().Get("dst")
	stream := streams.Get(dst)
	if stream == nil {
		http.Error(w, api.StreamNotFound, http.StatusNotFound)
		return
	}

	client, err := mpegts.Open(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stream.AddProducer(client)
	defer stream.RemoveProducer(client)

	if err = client.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}
