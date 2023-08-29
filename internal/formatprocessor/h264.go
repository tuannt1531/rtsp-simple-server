package formatprocessor

import (
	"bytes"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/unit"
)

// extract SPS and PPS without decoding RTP packets
func rtpH264ExtractSPSPPS(pkt *rtp.Packet) ([]byte, []byte) {
	if len(pkt.Payload) < 1 {
		return nil, nil
	}

	typ := h264.NALUType(pkt.Payload[0] & 0x1F)

	switch typ {
	case h264.NALUTypeSPS:
		return pkt.Payload, nil

	case h264.NALUTypePPS:
		return nil, pkt.Payload

	case h264.NALUTypeSTAPA:
		payload := pkt.Payload[1:]
		var sps []byte
		var pps []byte

		for len(payload) > 0 {
			if len(payload) < 2 {
				break
			}

			size := uint16(payload[0])<<8 | uint16(payload[1])
			payload = payload[2:]

			if size == 0 {
				break
			}

			if int(size) > len(payload) {
				return nil, nil
			}

			nalu := payload[:size]
			payload = payload[size:]

			typ = h264.NALUType(nalu[0] & 0x1F)

			switch typ {
			case h264.NALUTypeSPS:
				sps = nalu

			case h264.NALUTypePPS:
				pps = nalu
			}
		}

		return sps, pps

	default:
		return nil, nil
	}
}

type formatProcessorH264 struct {
	udpMaxPayloadSize int
	format            *format.H264

	encoder *rtph264.Encoder
	decoder *rtph264.Decoder
}

func newH264(
	udpMaxPayloadSize int,
	forma *format.H264,
	generateRTPPackets bool,
) (*formatProcessorH264, error) {
	t := &formatProcessorH264{
		udpMaxPayloadSize: udpMaxPayloadSize,
		format:            forma,
	}

	if generateRTPPackets {
		err := t.createEncoder(nil, nil)
		if err != nil {
			return nil, err
		}
	}

	return t, nil
}

func (t *formatProcessorH264) createEncoder(
	ssrc *uint32,
	initialSequenceNumber *uint16,
) error {
	t.encoder = &rtph264.Encoder{
		PayloadMaxSize:        t.udpMaxPayloadSize - 12,
		PayloadType:           t.format.PayloadTyp,
		SSRC:                  ssrc,
		InitialSequenceNumber: initialSequenceNumber,
		PacketizationMode:     t.format.PacketizationMode,
	}
	return t.encoder.Init()
}

func (t *formatProcessorH264) updateTrackParametersFromRTPPacket(pkt *rtp.Packet) {
	sps, pps := rtpH264ExtractSPSPPS(pkt)
	update := false

	if sps != nil && !bytes.Equal(sps, t.format.SPS) {
		update = true
	}

	if pps != nil && !bytes.Equal(pps, t.format.PPS) {
		update = true
	}

	if update {
		if sps == nil {
			sps = t.format.SPS
		}
		if pps == nil {
			pps = t.format.PPS
		}
		t.format.SafeSetParams(sps, pps)
	}
}

func (t *formatProcessorH264) updateTrackParametersFromAU(au [][]byte) {
	sps := t.format.SPS
	pps := t.format.PPS
	update := false

	for _, nalu := range au {
		typ := h264.NALUType(nalu[0] & 0x1F)

		switch typ {
		case h264.NALUTypeSPS:
			if !bytes.Equal(nalu, sps) {
				sps = nalu
				update = true
			}

		case h264.NALUTypePPS:
			if !bytes.Equal(nalu, pps) {
				pps = nalu
				update = true
			}
		}
	}

	if update {
		t.format.SafeSetParams(sps, pps)
	}
}

func (t *formatProcessorH264) remuxAccessUnit(au [][]byte) [][]byte {
	isKeyFrame := false
	n := 0

	for _, nalu := range au {
		typ := h264.NALUType(nalu[0] & 0x1F)

		switch typ {
		case h264.NALUTypeSPS, h264.NALUTypePPS: // parameters: remove
			continue

		case h264.NALUTypeAccessUnitDelimiter: // AUD: remove
			continue

		case h264.NALUTypeIDR: // key frame
			if !isKeyFrame {
				isKeyFrame = true

				// prepend parameters
				if t.format.SPS != nil && t.format.PPS != nil {
					n += 2
				}
			}
		}
		n++
	}

	if n == 0 {
		return nil
	}

	filteredNALUs := make([][]byte, n)
	i := 0

	if isKeyFrame && t.format.SPS != nil && t.format.PPS != nil {
		filteredNALUs[0] = t.format.SPS
		filteredNALUs[1] = t.format.PPS
		i = 2
	}

	for _, nalu := range au {
		typ := h264.NALUType(nalu[0] & 0x1F)

		switch typ {
		case h264.NALUTypeSPS, h264.NALUTypePPS:
			continue

		case h264.NALUTypeAccessUnitDelimiter:
			continue
		}

		filteredNALUs[i] = nalu
		i++
	}

	return filteredNALUs
}

func (t *formatProcessorH264) Process(u unit.Unit, hasNonRTSPReaders bool) error { //nolint:dupl
	tunit := u.(*unit.H264)

	if tunit.RTPPackets != nil {
		pkt := tunit.RTPPackets[0]
		t.updateTrackParametersFromRTPPacket(pkt)

		if t.encoder == nil {
			// remove padding
			pkt.Header.Padding = false
			pkt.PaddingSize = 0

			// RTP packets exceed maximum size: start re-encoding them
			if pkt.MarshalSize() > t.udpMaxPayloadSize {
				v1 := pkt.SSRC
				v2 := pkt.SequenceNumber
				err := t.createEncoder(&v1, &v2)
				if err != nil {
					return err
				}
			}
		}

		// decode from RTP
		if hasNonRTSPReaders || t.decoder != nil || t.encoder != nil {
			if t.decoder == nil {
				var err error
				t.decoder, err = t.format.CreateDecoder()
				if err != nil {
					return err
				}
			}

			au, err := t.decoder.Decode(pkt)
			if err != nil {
				if err == rtph264.ErrNonStartingPacketAndNoPrevious || err == rtph264.ErrMorePacketsNeeded {
					if t.encoder != nil {
						tunit.RTPPackets = nil
					}
					return nil
				}
				return err
			}

			tunit.AU = t.remuxAccessUnit(au)
		}

		// route packet as is
		if t.encoder == nil {
			return nil
		}
	} else {
		t.updateTrackParametersFromAU(tunit.AU)
		tunit.AU = t.remuxAccessUnit(tunit.AU)
	}

	// encode into RTP
	if len(tunit.AU) != 0 {
		pkts, err := t.encoder.Encode(tunit.AU)
		if err != nil {
			return err
		}
		setTimestamp(pkts, tunit.RTPPackets, t.format.ClockRate(), tunit.PTS)
		tunit.RTPPackets = pkts
	} else {
		tunit.RTPPackets = nil
	}

	return nil
}

func (t *formatProcessorH264) UnitForRTPPacket(pkt *rtp.Packet, ntp time.Time, pts time.Duration) Unit {
	return &unit.H264{
		Base: unit.Base{
			RTPPackets: []*rtp.Packet{pkt},
			NTP:        ntp,
			PTS:        pts,
		},
	}
}
