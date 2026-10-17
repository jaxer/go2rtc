package isapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
)

// Deprecated: should be rewritten to core.Connection
type Client struct {
	core.Listener

	url     string
	channel string
	conn    net.Conn

	medias  []*core.Media
	sender  *core.Sender
	senders []*core.Sender
	mixer   *speechMixer
	mu      sync.Mutex
	stopped bool
	opened  bool
	ctx     context.Context
	cancel  context.CancelFunc
	send    int
}

func Dial(rawURL string) (*Client, error) {
	// check if url is valid url
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	opts, err := url.ParseQuery(strings.ReplaceAll(u.Fragment, "#", "&"))
	if err != nil {
		return nil, err
	}
	if v := opts.Get("mix"); v != "" && v != "0" && v != "1" {
		return nil, errors.New("isapi: mix must be 0 or 1")
	}
	mixed := opts.Get("mix") == "1"
	gainDB := 0.0
	if v := opts.Get("gain"); v != "" {
		gainDB, err = strconv.ParseFloat(v, 64)
		if err != nil || !(gainDB >= 0 && gainDB <= 24) {
			return nil, errors.New("isapi: gain must be 0..24 dB")
		}
		if !mixed {
			return nil, errors.New("isapi: gain requires mix=1")
		}
	}
	targetRMS := 4096.0 // preserve the original approximately -18 dBFS default
	if v := opts.Get("target"); v != "" {
		dBFS, parseErr := strconv.ParseFloat(v, 64)
		if parseErr != nil || !(dBFS >= -24 && dBFS <= -6) {
			return nil, errors.New("isapi: target must be -24..-6 dBFS")
		}
		if !mixed || gainDB == 0 {
			return nil, errors.New("isapi: target requires mix=1 and positive gain")
		}
		targetRMS = 32768 * math.Pow(10, dBFS/20)
	}
	compression := opts.Get("compress")
	if compression != "" && compression != "moderate" {
		return nil, errors.New("isapi: compress must be moderate")
	}
	if compression != "" && !mixed {
		return nil, errors.New("isapi: compress requires mix=1")
	}
	u.Fragment = ""
	u.Scheme = "http"
	u.Path = ""

	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{url: u.String(), ctx: ctx, cancel: cancel}
	if err = client.Dial(); err != nil {
		cancel()
		return nil, err
	}
	if mixed {
		client.mixer = newSpeechMixer(client.medias[0].Codecs[0].Name, gainDB)
		client.mixer.targetRMS = targetRMS
		if compression == "moderate" {
			client.mixer.compression = moderateCompression(client.mixer.output)
		}
	}
	return client, nil
}

func (c *Client) Dial() (err error) {
	link := c.url + "/ISAPI/System/TwoWayAudio/channels"
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
	if err != nil {
		return err
	}

	res, err := tcp.Do(req)
	if err != nil {
		return
	}

	if res.StatusCode != http.StatusOK {
		tcp.Close(res)
		return errors.New(res.Status)
	}

	defer tcp.Close(res)
	b, err := io.ReadAll(io.LimitReader(res.Body, 65536))
	if err != nil {
		return err
	}

	xml := string(b)

	codec := core.Between(xml, `<audioCompressionType>`, `<`)
	switch codec {
	case "G.711ulaw":
		codec = core.CodecPCMU
	case "G.711alaw":
		codec = core.CodecPCMA
	default:
		return errors.New("isapi: unsupported audio codec")
	}

	c.channel = core.Between(xml, `<id>`, `<`)

	media := &core.Media{
		Kind:      core.KindAudio,
		Direction: core.DirectionSendonly,
		Codecs: []*core.Codec{
			{Name: codec, ClockRate: 8000},
		},
	}
	c.medias = append(c.medias, media)

	return nil
}

// Open owns a single device session for all microphones attached to this producer.
func (c *Client) Open() error {
	c.mu.Lock()
	stoppedBeforeStart := c.stopped
	c.mu.Unlock()
	if stoppedBeforeStart {
		return context.Canceled
	}
	// Preserve the existing recovery behavior only for non-mixing sources. A mixed
	// source must not forcibly close a stock call or another device's talk session.
	if c.mixer == nil {
		if err := c.command(c.ctx, "close"); err != nil {
			return err
		}
	}
	if err := c.claim(); err != nil {
		return err
	}
	c.mu.Lock()
	c.opened = true
	stopped := c.stopped
	c.mu.Unlock()
	openSessions.Store(c, struct{}{})
	defer func() {
		openSessions.Delete(c)
		c.mu.Lock()
		opened := c.opened
		c.opened = false
		c.mu.Unlock()
		if opened {
			_ = c.Close()
		}
	}()
	if stopped {
		return context.Canceled
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	conn, reader, err := c.openAudio(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return context.Canceled
	}
	c.conn = conn
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.conn = nil; c.mu.Unlock() }()
	done := make(chan struct{})
	if c.mixer != nil {
		go func() { defer close(done); c.mixTo(ctx, conn) }()
	} else {
		close(done)
	}
	// Discard any camera response/uplink bytes until disconnect; a response
	// body must not terminate the outgoing conversation.
	_, err = io.Copy(io.Discard, reader)
	conn.Close()
	// A write deadline also bounds the mixer when the peer stops reading.
	if c.mixer != nil {
		cancel()
		<-done
	}
	return err
}

// openSessions holds clients with an open talk session so a restart, which
// replaces the process without running deferred cleanup, can close them first.
var openSessions sync.Map

// CloseSessions closes every open talk session, at most 3 s per camera, in parallel.
func CloseSessions() {
	var wg sync.WaitGroup
	openSessions.Range(func(key, _ any) bool {
		wg.Add(1)
		go func() { defer wg.Done(); _ = key.(*Client).Close() }()
		return true
	})
	wg.Wait()
}

// errInvalidOperation is the camera refusing open because a talk session exists.
var errInvalidOperation = errors.New("isapi: talk channel busy")

// openFailures counts consecutive refused opens per camera. The stream worker
// restarts a failed producer immediately, so the delay lives here.
var openFailures sync.Map

// claim opens the talk channel. This process never holds a second session on a
// camera, so a refusal means a stale one (restart, lost close) or a real call.
// Reclaim it only when no intercom call is live: plain cameras answer 404.
func (c *Client) claim() error {
	if err := c.backoff(); err != nil {
		return err
	}
	err := c.command(c.ctx, "open")
	if errors.Is(err, errInvalidOperation) && c.callIdle() {
		if err = c.command(c.ctx, "close"); err == nil {
			err = c.command(c.ctx, "open")
		}
	}
	if err != nil {
		n, _ := openFailures.LoadOrStore(c.url, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		return err
	}
	openFailures.Delete(c.url)
	return nil
}

func (c *Client) backoff() error {
	n, ok := openFailures.Load(c.url)
	if !ok {
		return nil
	}
	delay := time.Second << min(n.(*atomic.Int32).Load()-1, 4)
	select {
	case <-time.After(min(delay, 10*time.Second)):
		return nil
	case <-c.ctx.Done():
		return context.Canceled
	}
}

func (c *Client) callIdle() bool {
	ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.url+"/ISAPI/VideoIntercom/callStatus?format=json", nil)
	if err != nil {
		return false
	}
	res, err := tcp.Do(req)
	if err != nil {
		return false
	}
	defer tcp.Close(res)
	if res.StatusCode == http.StatusNotFound {
		return true
	}
	var v struct{ CallStatus struct{ Status string } }
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&v) != nil {
		return false
	}
	return v.CallStatus.Status == "idle"
}

func (c *Client) command(parent context.Context, command string) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	link := c.url + "/ISAPI/System/TwoWayAudio/channels/" + c.channel + "/" + command
	req, err := http.NewRequestWithContext(ctx, "PUT", link, nil)
	if err != nil {
		return err
	}
	res, err := tcp.Do(req)
	if err != nil {
		return err
	}
	defer tcp.Close(res)
	body, err := io.ReadAll(io.LimitReader(res.Body, 65536))
	if err != nil {
		return err
	}
	code := core.Between(string(body), "<statusCode>", "<")
	if res.StatusCode != 200 || (code != "" && code != "1") {
		err = fmt.Errorf("isapi: %s HTTP %d status %s", command, res.StatusCode, code)
		if code == "4" {
			err = fmt.Errorf("%w: %w", err, errInvalidOperation)
		}
		return err
	}
	return nil
}

func (c *Client) Close() error { return c.command(context.Background(), "close") }

func (c *Client) mixTo(ctx context.Context, conn net.Conn) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var out [frameSamples]byte
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			stopped := c.stopped
			c.mu.Unlock()
			if stopped {
				return
			}
			if !c.mixer.read(out[:], time.Now()) {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(750 * time.Millisecond))
			n, err := conn.Write(out[:])
			c.mu.Lock()
			c.send += n
			c.mu.Unlock()
			if err != nil || n != len(out) {
				conn.Close()
				return
			}
		}
	}
}
