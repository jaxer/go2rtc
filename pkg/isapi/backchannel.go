package isapi

import (
	"encoding/json"
	"errors"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

func (c *Client) GetMedias() []*core.Media {
	return c.medias
}

func (c *Client) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return nil, core.ErrCantGetTrack
}

func (c *Client) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return errors.New("isapi: producer stopped")
	}
	if c.mixer != nil {
		sender, err := c.mixer.add(media, track)
		if err != nil {
			return err
		}
		for _, existing := range c.senders {
			if existing == sender {
				return nil
			}
		}
		// Keep only current participants, not every historical attachment.
		c.mixer.mu.Lock()
		c.senders = c.senders[:0]
		for _, input := range c.mixer.inputs {
			c.senders = append(c.senders, input.sender)
		}
		c.mixer.mu.Unlock()
		return nil
	}
	if c.sender == nil {
		c.sender = core.NewSender(media, track.Codec)
		c.sender.Handler = func(packet *rtp.Packet) {
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				return
			}
			n, _ := conn.Write(packet.Payload)
			c.mu.Lock()
			c.send += n
			c.mu.Unlock()
		}
	}

	c.sender.HandleRTP(track)
	return nil
}

func (c *Client) Start() (err error) {
	if err = c.Open(); err != nil {
		return
	}
	return
}

func (c *Client) Stop() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	conn := c.conn
	senders := c.senders
	sender := c.sender
	c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	for _, s := range senders {
		s.Close()
	}
	if sender != nil {
		sender.Close()
	}
	return nil
}

func (c *Client) MarshalJSON() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := &core.Connection{
		ID:         core.ID(c),
		FormatName: "isapi",
		Protocol:   "http",
		Medias:     c.medias,
		Send:       c.send,
	}
	if c.conn != nil {
		info.RemoteAddr = c.conn.RemoteAddr().String()
	}
	if c.mixer != nil {
		info.Senders = append(info.Senders, c.senders...)
	}
	if c.sender != nil {
		info.Senders = []*core.Sender{c.sender}
	}
	return json.Marshal(info)
}
