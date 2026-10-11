package isapi

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// Identical camera URLs across low/high streams share one physical audio session.
// Credentials and gain must agree; DNS aliases for one camera must be configured
// with the same canonical hostname. Nothing is pooled for legacy sources.
var sharedCameras = struct {
	sync.Mutex
	cameras map[string]*sharedCamera
}{cameras: make(map[string]*sharedCamera)}

type sharedCamera struct {
	client  *Client
	config  string
	refs    int
	tracks  map[*core.Receiver]int
	once    sync.Once
	done    chan struct{}
	err     error
	closing bool
	retired chan struct{}
}
type sharedClient struct {
	camera  *sharedCamera
	key     string
	tracks  map[*core.Receiver]bool
	stopped chan struct{}
	closed  bool // protected by sharedCameras
}

func DialShared(raw string) (core.Producer, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	options, err := url.ParseQuery(strings.ReplaceAll(u.Fragment, "#", "&"))
	if err != nil {
		return nil, err
	}
	if options.Get("mix") != "1" {
		return Dial(raw)
	}
	// Keep secrets only in the consistency check, never in the camera key/errors.
	config := u.User.String() + "#" + options.Encode()
	key := strings.ToLower(u.Host)
	key = strings.TrimSuffix(key, ":80")
	var candidate *Client
	defer func() {
		if candidate != nil {
			candidate.Stop()
		}
	}()
	for {
		sharedCameras.Lock()
		camera := sharedCameras.cameras[key]
		if camera != nil && camera.closing {
			retired := camera.retired
			sharedCameras.Unlock()
			<-retired
			continue
		}
		if camera != nil {
			select {
			case <-camera.done:
				// Start has returned after device cleanup. Existing views can release the
				// failed generation while reconnecting views acquire a fresh one.
				delete(sharedCameras.cameras, key)
				camera = nil
			default:
			}
		}
		if camera == nil && candidate == nil {
			sharedCameras.Unlock()
			candidate, err = Dial(raw)
			if err != nil {
				return nil, err
			}
			continue
		}
		if camera == nil {
			camera = &sharedCamera{client: candidate, config: config, tracks: make(map[*core.Receiver]int), done: make(chan struct{}), retired: make(chan struct{})}
			candidate = nil
			sharedCameras.cameras[key] = camera
		} else if camera.config != config {
			sharedCameras.Unlock()
			return nil, errors.New("isapi: shared camera credentials/mixer options must agree")
		}
		camera.refs++
		client := &sharedClient{camera: camera, key: key, tracks: make(map[*core.Receiver]bool), stopped: make(chan struct{})}
		sharedCameras.Unlock()
		return client, nil
	}

}
func (c *sharedClient) GetMedias() []*core.Media { return c.camera.client.GetMedias() }
func (c *sharedClient) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return nil, core.ErrCantGetTrack
}
func (c *sharedClient) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	sharedCameras.Lock()
	defer sharedCameras.Unlock()
	if c.closed {
		return errors.New("isapi: shared producer stopped")
	}
	if c.tracks[track] {
		return nil
	}
	if err := c.camera.client.AddTrack(media, codec, track); err != nil {
		return err
	}
	c.tracks[track] = true
	c.camera.tracks[track]++
	return nil
}
func (c *sharedClient) Start() error {
	sharedCameras.Lock()
	if c.closed {
		sharedCameras.Unlock()
		return nil
	}
	camera := c.camera
	camera.once.Do(func() { go func() { camera.err = camera.client.Start(); close(camera.done) }() })
	sharedCameras.Unlock()
	select {
	case <-c.stopped:
		return nil
	case <-camera.done:
		return camera.err
	}
}
func (c *sharedClient) Stop() error {
	sharedCameras.Lock()
	if c.closed {
		sharedCameras.Unlock()
		return nil
	}
	c.closed = true
	close(c.stopped)
	camera := c.camera
	for track := range c.tracks {
		camera.tracks[track]--
		if camera.tracks[track] == 0 {
			delete(camera.tracks, track)
			camera.client.removeTrack(track)
		}
	}
	camera.refs--
	last := camera.refs == 0
	if last {
		camera.closing = true
	}
	sharedCameras.Unlock()
	if last {
		camera.client.Stop()
		// No global lock around device I/O: another camera can start immediately.
		camera.once.Do(func() { close(camera.done) })
		<-camera.done
		sharedCameras.Lock()
		if sharedCameras.cameras[c.key] == camera {
			delete(sharedCameras.cameras, c.key)
		}
		close(camera.retired)
		sharedCameras.Unlock()
	}
	return nil
}
func (c *sharedClient) MarshalJSON() ([]byte, error) { return json.Marshal(c.camera.client) }

func (c *Client) removeTrack(track *core.Receiver) {
	c.mu.Lock()
	c.mixer.mu.Lock()
	input := c.mixer.inputs[track]
	delete(c.mixer.inputs, track)
	c.mixer.mu.Unlock()
	if input != nil {
		for i, s := range c.senders {
			if s == input.sender {
				c.senders = append(c.senders[:i], c.senders[i+1:]...)
				break
			}
		}
	}
	c.mu.Unlock()
	if input != nil {
		input.sender.Close()
	}
}
