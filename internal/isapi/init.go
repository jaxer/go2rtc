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
		return &logged{Producer: prod, camera: host(source)}, nil
	})
}

// logged reports how every talk session ends: a session that keeps dropping and
// reopening shows up in the log with its reason instead of as silent HTTP churn.
type logged struct {
	core.Producer
	camera string
}

func (l *logged) Start() error {
	started := time.Now()
	err := l.Producer.Start()
	log.Info().Err(err).Str("camera", l.camera).Dur("after", time.Since(started)).Msg("[isapi] talk session ended")
	return err
}

func (l *logged) MarshalJSON() ([]byte, error) {
	if m, ok := l.Producer.(interface{ MarshalJSON() ([]byte, error) }); ok {
		return m.MarshalJSON()
	}
	return nil, nil
}

// host keeps credentials out of the log.
func host(source string) string {
	if u, err := url.Parse(source); err == nil {
		return u.Host
	}
	return "?"
}
