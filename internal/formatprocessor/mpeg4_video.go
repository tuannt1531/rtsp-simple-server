package formatprocessor //nolint:dupl

import (
	"bytes"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtpmpeg4video"
	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg4video"
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/unit"
)

type formatProcessorMPEG4Video struct {
	udpMaxPayloadSize int
	format            *format.MPEG4Video
	encoder           *rtpmpeg4video.Encoder
	decoder           *rtpmpeg4video.Decoder
}

func newMPEG4Video(
	udpMaxPayloadSize int,
	forma *format.MPEG4Video,
	generateRTPPackets bool,
) (*formatProcessorMPEG4Video, error) {
	t := &formatProcessorMPEG4Video{
		udpMaxPayloadSize: udpMaxPayloadSize,
		format:            forma,
	}

	if generateRTPPackets {
		err := t.createEncoder()
		if err != nil {
			return nil, err
		}
	}

	return t, nil
}

func (t *formatProcessorMPEG4Video) createEncoder() error {
	t.encoder = &rtpmpeg4video.Encoder{
		PayloadMaxSize: t.udpMaxPayloadSize - 12,
		PayloadType:    t.format.PayloadTyp,
	}
	return t.encoder.Init()
}

func (t *formatProcessorMPEG4Video) updateTrackParameters(frame []byte) {
	if bytes.HasPrefix(frame, []byte{0, 0, 1, byte(mpeg4video.VisualObjectSequenceStartCode)}) {
		end := bytes.Index(frame[4:], []byte{0, 0, 1, byte(mpeg4video.GroupOfVOPStartCode)})
		if end < 0 {
			return
		}
		conf := frame[:end+4]

		if !bytes.Equal(conf, t.format.Config) {
			t.format.SafeSetParams(conf)
		}
	}
}

func (t *formatProcessorMPEG4Video) remuxFrame(frame []byte) []byte {
	if bytes.HasPrefix(frame, []byte{0, 0, 1, byte(mpeg4video.VisualObjectSequenceStartCode)}) {
		end := bytes.Index(frame[4:], []byte{0, 0, 1, byte(mpeg4video.GroupOfVOPStartCode)})
		if end >= 0 {
			frame = frame[end+4:]
		}
	}

	if bytes.Contains(frame, []byte{0, 0, 1, byte(mpeg4video.GroupOfVOPStartCode)}) {
		f := make([]byte, len(t.format.Config)+len(frame))
		n := copy(f, t.format.Config)
		copy(f[n:], frame)
		frame = f
	}

	return frame
}

func (t *formatProcessorMPEG4Video) ProcessUnit(uu unit.Unit) error { //nolint:dupl
	u := uu.(*unit.MPEG4Video)

	t.updateTrackParameters(u.Frame)
	u.Frame = t.remuxFrame(u.Frame)

	if len(u.Frame) != 0 {
		pkts, err := t.encoder.Encode(u.Frame)
		if err != nil {
			return err
		}

		ts := uint32(multiplyAndDivide(u.PTS, time.Duration(t.format.ClockRate()), time.Second))
		for _, pkt := range pkts {
			pkt.Timestamp += ts
		}

		u.RTPPackets = pkts
	}

	return nil
}

func (t *formatProcessorMPEG4Video) ProcessRTPPacket( //nolint:dupl
	pkt *rtp.Packet,
	ntp time.Time,
	pts time.Duration,
	hasNonRTSPReaders bool,
) (Unit, error) {
	u := &unit.MPEG4Video{
		Base: unit.Base{
			RTPPackets: []*rtp.Packet{pkt},
			NTP:        ntp,
			PTS:        pts,
		},
	}

	t.updateTrackParameters(pkt.Payload)

	// remove padding
	pkt.Header.Padding = false
	pkt.PaddingSize = 0

	if pkt.MarshalSize() > t.udpMaxPayloadSize {
		return nil, fmt.Errorf("payload size (%d) is greater than maximum allowed (%d)",
			pkt.MarshalSize(), t.udpMaxPayloadSize)
	}

	// decode from RTP
	if hasNonRTSPReaders || t.decoder != nil {
		if t.decoder == nil {
			var err error
			t.decoder, err = t.format.CreateDecoder()
			if err != nil {
				return nil, err
			}
		}

		frame, err := t.decoder.Decode(pkt)
		if err != nil {
			if err == rtpmpeg4video.ErrMorePacketsNeeded {
				return u, nil
			}
			return nil, err
		}

		u.Frame = t.remuxFrame(frame)
	}

	// route packet as is
	return u, nil
}
