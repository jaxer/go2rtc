package isapi

import (
	"net/url"
	"time"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/isapi"
	"github.com/rs/zerolog"
)

var log zerolog.Logger

func Init() {
	log = app.GetLogger("isapi")

	// A session that keeps dropping and reopening shows up here with its reason
	// instead of as silent HTTP churn on the camera.
	isapi.OnSessionEnd = func(camera string, lived time.Duration, err error) {
		log.Info().Err(err).Str("camera", camera).Dur("after", lived).Msg("[isapi] talk session ended")
	}

	// The producer is returned as is: streams type-asserts it as core.Consumer for
	// backchannel tracks, so no wrapper may stand in between (7a19b93 panicked on one).
	streams.HandleFunc("isapi", func(source string) (core.Producer, error) {
		started := time.Now()
		prod, err := isapi.DialShared(source)
		if err != nil {
			log.Warn().Err(err).Str("camera", host(source)).Dur("after", time.Since(started)).Msg("[isapi] dial failed")
			return nil, err
		}
		if time.Since(started) > time.Second {
			log.Warn().Str("camera", host(source)).Dur("after", time.Since(started)).Msg("[isapi] slow dial")
		}
		return prod, nil
	})
}

// host keeps credentials out of the log.
func host(source string) string {
	if u, err := url.Parse(source); err == nil {
		return u.Host
	}
	return "?"
}
