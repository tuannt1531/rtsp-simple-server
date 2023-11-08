package formatprocessor //nolint:dupl

import (
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtpvp9"
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/unit"
)

type formatProcessorVP9 struct {
	udpMaxPayloadSize int
	format            *format.VP9
	encoder           *rtpvp9.Encoder
	decoder           *rtpvp9.Decoder
}

func newVP9(
	udpMaxPayloadSize int,
	forma *format.VP9,
	generateRTPPackets bool,
) (*formatProcessorVP9, error) {
	t := &formatProcessorVP9{
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

func (t *formatProcessorVP9) createEncoder() error {
	t.encoder = &rtpvp9.Encoder{
		PayloadMaxSize: t.udpMaxPayloadSize - 12,
		PayloadType:    t.format.PayloadTyp,
	}
	return t.encoder.Init()
}

func (t *formatProcessorVP9) ProcessUnit(uu unit.Unit) error { //nolint:dupl
	u := uu.(*unit.VP9)

	pkts, err := t.encoder.Encode(u.Frame)
	if err != nil {
		return err
	}

	ts := uint32(multiplyAndDivide(u.PTS, time.Duration(t.format.ClockRate()), time.Second))
	for _, pkt := range pkts {
		pkt.Timestamp += ts
	}

	u.RTPPackets = pkts

	return nil
}

func (t *formatProcessorVP9) ProcessRTPPacket( //nolint:dupl
	pkt *rtp.Packet,
	ntp time.Time,
	pts time.Duration,
	hasNonRTSPReaders bool,
) (Unit, error) {
	u := &unit.VP9{
		Base: unit.Base{
			RTPPackets: []*rtp.Packet{pkt},
			NTP:        ntp,
			PTS:        pts,
		},
	}

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
			if err == rtpvp9.ErrNonStartingPacketAndNoPrevious || err == rtpvp9.ErrMorePacketsNeeded {
				return u, nil
			}
			return nil, err
		}

		u.Frame = frame
	}

	// route packet as is
	return u, nil
}
