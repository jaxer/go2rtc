package mpegts

import (
	"io"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/pion/rtp"
)

type Consumer struct {
	core.Connection
	muxer *Muxer
	wr    *core.WriteBuffer
}

func NewConsumer() *Consumer {
	medias := []*core.Media{
		{
			Kind:      core.KindVideo,
			Direction: core.DirectionSendonly,
			Codecs: []*core.Codec{
				{Name: core.CodecH264},
				{Name: core.CodecH265},
			},
		},
		{
			Kind:      core.KindAudio,
			Direction: core.DirectionSendonly,
			Codecs: []*core.Codec{
				{Name: core.CodecAAC},
			},
		},
	}
	wr := core.NewWriteBuffer(nil)
	return &Consumer{
		core.Connection{
			ID:         core.NewID(),
			FormatName: "mpegts",
			Medias:     medias,
			Transport:  wr,
		},
		NewMuxer(),
		wr,
	}
}

// WithG711 also accepts PCMU and PCMA audio. Off by default, so a plain stream.ts request
// keeps its exact output; standard players ignore these private stream types.
func (c *Consumer) WithG711(pcmu, pcma bool) {
	for _, media := range c.Medias {
		if media.Kind != core.KindAudio {
			continue
		}
		if pcmu {
			media.Codecs = append(media.Codecs, &core.Codec{Name: core.CodecPCMU})
		}
		if pcma {
			media.Codecs = append(media.Codecs, &core.Codec{Name: core.CodecPCMA})
		}
	}
}

func (c *Consumer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, track.Codec)

	switch track.Codec.Name {
	case core.CodecH264:
		pid := c.muxer.AddTrack(StreamTypeH264)

		sender.Handler = func(pkt *rtp.Packet) {
			b := c.muxer.GetPayload(pid, pkt.Timestamp, pkt.Payload)
			if n, err := c.wr.Write(b); err == nil {
				c.Send += n
			}
		}

		if track.Codec.IsRTP() {
			sender.Handler = h264.RTPDepay(track.Codec, sender.Handler)
		} else {
			sender.Handler = h264.RepairAVCC(track.Codec, sender.Handler)
		}

	case core.CodecH265:
		pid := c.muxer.AddTrack(StreamTypeH265)

		sender.Handler = func(pkt *rtp.Packet) {
			b := c.muxer.GetPayload(pid, pkt.Timestamp, pkt.Payload)
			if n, err := c.wr.Write(b); err == nil {
				c.Send += n
			}
		}

		if track.Codec.IsRTP() {
			sender.Handler = h265.RTPDepay(track.Codec, sender.Handler)
		}

	case core.CodecPCMU, core.CodecPCMA:
		// G.711 as the Tapo-style private stream types the demuxer already reads (0x91, 0x90):
		// the RTP payload as is, one PES per packet. Only offered when asked for, see WithG711.
		st := byte(StreamTypePCMUTapo)
		if track.Codec.Name == core.CodecPCMA {
			st = StreamTypePCMATapo
		}
		pid := c.muxer.AddTrack(st)

		// convert timestamp to 90000Hz clock
		dt := 90000 / float64(track.Codec.ClockRate)

		sender.Handler = func(pkt *rtp.Packet) {
			pts := uint32(float64(pkt.Timestamp) * dt)
			b := c.muxer.GetPayload(pid, pts, pkt.Payload)
			if n, err := c.wr.Write(b); err == nil {
				c.Send += n
			}
		}

	case core.CodecAAC:
		pid := c.muxer.AddTrack(StreamTypeAAC)

		// convert timestamp to 90000Hz clock
		dt := 90000 / float64(track.Codec.ClockRate)

		sender.Handler = func(pkt *rtp.Packet) {
			pts := uint32(float64(pkt.Timestamp) * dt)
			b := c.muxer.GetPayload(pid, pts, pkt.Payload)
			if n, err := c.wr.Write(b); err == nil {
				c.Send += n
			}
		}

		if track.Codec.IsRTP() {
			sender.Handler = aac.RTPToADTS(track.Codec, sender.Handler)
		} else {
			sender.Handler = aac.EncodeToADTS(track.Codec, sender.Handler)
		}
	}

	sender.HandleRTP(track)
	c.Senders = append(c.Senders, sender)
	return nil
}

func (c *Consumer) WriteTo(wr io.Writer) (int64, error) {
	b := c.muxer.GetHeader()
	if _, err := wr.Write(b); err != nil {
		return 0, err
	}

	return c.wr.WriteTo(wr)
}

//func TimestampFromRTP(rtp *rtp.Packet, codec *core.Codec) {
//	if codec.ClockRate == ClockRate {
//		return
//	}
//	rtp.Timestamp = uint32(float64(rtp.Timestamp) / float64(codec.ClockRate) * ClockRate)
//}
