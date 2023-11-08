package record

import (
	"bytes"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/mediacommon/pkg/codecs/ac3"
	"github.com/bluenviron/mediacommon/pkg/codecs/av1"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/pkg/codecs/jpeg"
	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg4video"
	"github.com/bluenviron/mediacommon/pkg/codecs/opus"
	"github.com/bluenviron/mediacommon/pkg/codecs/vp9"
	"github.com/bluenviron/mediacommon/pkg/formats/fmp4"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/unit"
)

func durationGoToMp4(v time.Duration, timeScale uint32) uint64 {
	timeScale64 := uint64(timeScale)
	secs := v / time.Second
	dec := v % time.Second
	return uint64(secs)*timeScale64 + uint64(dec)*timeScale64/uint64(time.Second)
}

func mpeg1audioChannelCount(cm mpeg1audio.ChannelMode) int {
	switch cm {
	case mpeg1audio.ChannelModeStereo,
		mpeg1audio.ChannelModeJointStereo,
		mpeg1audio.ChannelModeDualChannel:
		return 2

	default:
		return 1
	}
}

func jpegExtractSize(image []byte) (int, int, error) {
	l := len(image)
	if l < 2 || image[0] != 0xFF || image[1] != jpeg.MarkerStartOfImage {
		return 0, 0, fmt.Errorf("invalid header")
	}

	image = image[2:]

	for {
		if len(image) < 2 {
			return 0, 0, fmt.Errorf("not enough bits")
		}

		h0, h1 := image[0], image[1]
		image = image[2:]

		if h0 != 0xFF {
			return 0, 0, fmt.Errorf("invalid image")
		}

		switch h1 {
		case 0xE0, 0xE1, 0xE2, // JFIF
			jpeg.MarkerDefineHuffmanTable,
			jpeg.MarkerComment,
			jpeg.MarkerDefineQuantizationTable,
			jpeg.MarkerDefineRestartInterval:
			mlen := int(image[0])<<8 | int(image[1])
			if len(image) < mlen {
				return 0, 0, fmt.Errorf("not enough bits")
			}
			image = image[mlen:]

		case jpeg.MarkerStartOfFrame1:
			mlen := int(image[0])<<8 | int(image[1])
			if len(image) < mlen {
				return 0, 0, fmt.Errorf("not enough bits")
			}

			var sof jpeg.StartOfFrame1
			err := sof.Unmarshal(image[2:mlen])
			if err != nil {
				return 0, 0, err
			}

			return sof.Width, sof.Height, nil

		case jpeg.MarkerStartOfScan:
			return 0, 0, fmt.Errorf("SOF not found")

		default:
			return 0, 0, fmt.Errorf("unknown marker: 0x%.2x", h1)
		}
	}
}

type recFormatFMP4 struct {
	a *agentInstance

	tracks             []*recFormatFMP4Track
	hasVideo           bool
	currentSegment     *recFormatFMP4Segment
	nextSequenceNumber uint32
}

func (f *recFormatFMP4) initialize() {
	nextID := 1

	addTrack := func(codec fmp4.Codec) *recFormatFMP4Track {
		initTrack := &fmp4.InitTrack{
			TimeScale: 90000,
			Codec:     codec,
		}
		initTrack.ID = nextID
		nextID++

		track := newRecFormatFMP4Track(f, initTrack)
		f.tracks = append(f.tracks, track)

		return track
	}

	updateCodecs := func() {
		// if codec parameters have been updated,
		// and current segment has already written codec parameters on disk,
		// close current segment.
		if f.currentSegment != nil && f.currentSegment.fi != nil {
			f.currentSegment.close() //nolint:errcheck
			f.currentSegment = nil
		}
	}

	for _, media := range f.a.wrapper.Stream.Desc().Medias {
		for _, forma := range media.Formats {
			switch forma := forma.(type) {
			case *format.AV1:
				codec := &fmp4.CodecAV1{
					SequenceHeader: []byte{
						8, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64,
					},
				}
				track := addTrack(codec)

				firstReceived := false

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.AV1)
					if tunit.TU == nil {
						return nil
					}

					randomAccess := false

					for _, obu := range tunit.TU {
						var h av1.OBUHeader
						err := h.Unmarshal(obu)
						if err != nil {
							return err
						}

						if h.Type == av1.OBUTypeSequenceHeader {
							if !bytes.Equal(codec.SequenceHeader, obu) {
								codec.SequenceHeader = obu
								updateCodecs()
							}
							randomAccess = true
						}
					}

					if !firstReceived {
						if !randomAccess {
							return nil
						}
						firstReceived = true
					}

					sampl, err := fmp4.NewPartSampleAV1(
						randomAccess,
						tunit.TU)
					if err != nil {
						return err
					}

					return track.record(&sample{
						PartSample: sampl,
						dts:        tunit.PTS,
					})
				})

			case *format.VP9:
				codec := &fmp4.CodecVP9{
					Width:             1280,
					Height:            720,
					Profile:           1,
					BitDepth:          8,
					ChromaSubsampling: 1,
					ColorRange:        false,
				}
				track := addTrack(codec)

				firstReceived := false

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.VP9)
					if tunit.Frame == nil {
						return nil
					}

					var h vp9.Header
					err := h.Unmarshal(tunit.Frame)
					if err != nil {
						return err
					}

					randomAccess := false

					if h.FrameType == vp9.FrameTypeKeyFrame {
						randomAccess = true

						if w := h.Width(); codec.Width != w {
							codec.Width = w
							updateCodecs()
						}
						if h := h.Width(); codec.Height != h {
							codec.Height = h
							updateCodecs()
						}
						if codec.Profile != h.Profile {
							codec.Profile = h.Profile
							updateCodecs()
						}
						if codec.BitDepth != h.ColorConfig.BitDepth {
							codec.BitDepth = h.ColorConfig.BitDepth
							updateCodecs()
						}
						if c := h.ChromaSubsampling(); codec.ChromaSubsampling != c {
							codec.ChromaSubsampling = c
							updateCodecs()
						}
						if codec.ColorRange != h.ColorConfig.ColorRange {
							codec.ColorRange = h.ColorConfig.ColorRange
							updateCodecs()
						}
					}

					if !firstReceived {
						if !randomAccess {
							return nil
						}
						firstReceived = true
					}

					return track.record(&sample{
						PartSample: &fmp4.PartSample{
							IsNonSyncSample: !randomAccess,
							Payload:         tunit.Frame,
						},
						dts: tunit.PTS,
					})
				})

			case *format.VP8:
				// TODO

			case *format.H265:
				vps, sps, pps := forma.SafeParams()

				if vps == nil || sps == nil || pps == nil {
					vps = []byte{
						0x40, 0x01, 0x0c, 0x01, 0xff, 0xff, 0x02, 0x20,
						0x00, 0x00, 0x03, 0x00, 0xb0, 0x00, 0x00, 0x03,
						0x00, 0x00, 0x03, 0x00, 0x7b, 0x18, 0xb0, 0x24,
					}

					sps = []byte{
						0x42, 0x01, 0x01, 0x02, 0x20, 0x00, 0x00, 0x03,
						0x00, 0xb0, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03,
						0x00, 0x7b, 0xa0, 0x07, 0x82, 0x00, 0x88, 0x7d,
						0xb6, 0x71, 0x8b, 0x92, 0x44, 0x80, 0x53, 0x88,
						0x88, 0x92, 0xcf, 0x24, 0xa6, 0x92, 0x72, 0xc9,
						0x12, 0x49, 0x22, 0xdc, 0x91, 0xaa, 0x48, 0xfc,
						0xa2, 0x23, 0xff, 0x00, 0x01, 0x00, 0x01, 0x6a,
						0x02, 0x02, 0x02, 0x01,
					}

					pps = []byte{
						0x44, 0x01, 0xc0, 0x25, 0x2f, 0x05, 0x32, 0x40,
					}
				}

				codec := &fmp4.CodecH265{
					VPS: vps,
					SPS: sps,
					PPS: pps,
				}
				track := addTrack(codec)

				var dtsExtractor *h265.DTSExtractor

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.H265)
					if tunit.AU == nil {
						return nil
					}

					randomAccess := false

					for _, nalu := range tunit.AU {
						typ := h265.NALUType((nalu[0] >> 1) & 0b111111)

						switch typ {
						case h265.NALUType_VPS_NUT:
							if !bytes.Equal(codec.VPS, nalu) {
								codec.VPS = nalu
								updateCodecs()
							}

						case h265.NALUType_SPS_NUT:
							if !bytes.Equal(codec.SPS, nalu) {
								codec.SPS = nalu
								updateCodecs()
							}

						case h265.NALUType_PPS_NUT:
							if !bytes.Equal(codec.PPS, nalu) {
								codec.PPS = nalu
								updateCodecs()
							}

						case h265.NALUType_IDR_W_RADL, h265.NALUType_IDR_N_LP, h265.NALUType_CRA_NUT:
							randomAccess = true
						}
					}

					if dtsExtractor == nil {
						if !randomAccess {
							return nil
						}
						dtsExtractor = h265.NewDTSExtractor()
					}

					dts, err := dtsExtractor.Extract(tunit.AU, tunit.PTS)
					if err != nil {
						return err
					}

					sampl, err := fmp4.NewPartSampleH26x(
						int32(durationGoToMp4(tunit.PTS-dts, 90000)),
						randomAccess,
						tunit.AU)
					if err != nil {
						return err
					}

					return track.record(&sample{
						PartSample: sampl,
						dts:        dts,
					})
				})

			case *format.H264:
				sps, pps := forma.SafeParams()

				if sps == nil || pps == nil {
					sps = []byte{
						0x67, 0x42, 0xc0, 0x1f, 0xd9, 0x00, 0xf0, 0x11,
						0x7e, 0xf0, 0x11, 0x00, 0x00, 0x03, 0x00, 0x01,
						0x00, 0x00, 0x03, 0x00, 0x30, 0x8f, 0x18, 0x32,
						0x48,
					}

					pps = []byte{
						0x68, 0xcb, 0x8c, 0xb2,
					}
				}

				codec := &fmp4.CodecH264{
					SPS: sps,
					PPS: pps,
				}
				track := addTrack(codec)

				var dtsExtractor *h264.DTSExtractor

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.H264)
					if tunit.AU == nil {
						return nil
					}

					randomAccess := false

					for _, nalu := range tunit.AU {
						typ := h264.NALUType(nalu[0] & 0x1F)
						switch typ {
						case h264.NALUTypeSPS:
							if !bytes.Equal(codec.SPS, nalu) {
								codec.SPS = nalu
								updateCodecs()
							}

						case h264.NALUTypePPS:
							if !bytes.Equal(codec.PPS, nalu) {
								codec.PPS = nalu
								updateCodecs()
							}

						case h264.NALUTypeIDR:
							randomAccess = true
						}
					}

					if dtsExtractor == nil {
						if !randomAccess {
							return nil
						}
						dtsExtractor = h264.NewDTSExtractor()
					}

					dts, err := dtsExtractor.Extract(tunit.AU, tunit.PTS)
					if err != nil {
						return err
					}

					sampl, err := fmp4.NewPartSampleH26x(
						int32(durationGoToMp4(tunit.PTS-dts, 90000)),
						randomAccess,
						tunit.AU)
					if err != nil {
						return err
					}

					return track.record(&sample{
						PartSample: sampl,
						dts:        dts,
					})
				})

			case *format.MPEG4Video:
				config := forma.SafeParams()

				if config == nil {
					config = []byte{
						0x00, 0x00, 0x01, 0xb0, 0x01, 0x00, 0x00, 0x01,
						0xb5, 0x89, 0x13, 0x00, 0x00, 0x01, 0x00, 0x00,
						0x00, 0x01, 0x20, 0x00, 0xc4, 0x8d, 0x88, 0x00,
						0xf5, 0x3c, 0x04, 0x87, 0x14, 0x63, 0x00, 0x00,
						0x01, 0xb2, 0x4c, 0x61, 0x76, 0x63, 0x35, 0x38,
						0x2e, 0x31, 0x33, 0x34, 0x2e, 0x31, 0x30, 0x30,
					}
				}

				codec := &fmp4.CodecMPEG4Video{
					Config: config,
				}
				track := addTrack(codec)

				firstReceived := false
				var lastPTS time.Duration

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.MPEG4Video)
					if tunit.Frame == nil {
						return nil
					}

					randomAccess := bytes.Contains(tunit.Frame, []byte{0, 0, 1, byte(mpeg4video.GroupOfVOPStartCode)})

					if bytes.HasPrefix(tunit.Frame, []byte{0, 0, 1, byte(mpeg4video.VisualObjectSequenceStartCode)}) {
						end := bytes.Index(tunit.Frame[4:], []byte{0, 0, 1, byte(mpeg4video.GroupOfVOPStartCode)})
						if end >= 0 {
							config := tunit.Frame[:end+4]

							if !bytes.Equal(codec.Config, config) {
								codec.Config = config
								updateCodecs()
							}
						}
					}

					if !firstReceived {
						if !randomAccess {
							return nil
						}
						firstReceived = true
					} else if tunit.PTS < lastPTS {
						return fmt.Errorf("MPEG-4 Video streams with B-frames are not supported (yet)")
					}
					lastPTS = tunit.PTS

					return track.record(&sample{
						PartSample: &fmp4.PartSample{
							Payload:         tunit.Frame,
							IsNonSyncSample: !randomAccess,
						},
						dts: tunit.PTS,
					})
				})

			case *format.MPEG1Video:
				codec := &fmp4.CodecMPEG1Video{
					Config: []byte{
						0x00, 0x00, 0x01, 0xb3, 0x78, 0x04, 0x38, 0x35,
						0xff, 0xff, 0xe0, 0x18, 0x00, 0x00, 0x01, 0xb5,
						0x14, 0x4a, 0x00, 0x01, 0x00, 0x00,
					},
				}
				track := addTrack(codec)

				firstReceived := false
				var lastPTS time.Duration

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.MPEG1Video)
					if tunit.Frame == nil {
						return nil
					}

					randomAccess := bytes.Contains(tunit.Frame, []byte{0, 0, 1, 0xB8})

					if bytes.HasPrefix(tunit.Frame, []byte{0, 0, 1, 0xB3}) {
						end := bytes.Index(tunit.Frame[4:], []byte{0, 0, 1, 0xB8})
						if end >= 0 {
							config := tunit.Frame[:end+4]

							if !bytes.Equal(codec.Config, config) {
								codec.Config = config
								updateCodecs()
							}
						}
					}

					if !firstReceived {
						if !randomAccess {
							return nil
						}
						firstReceived = true
					} else if tunit.PTS < lastPTS {
						return fmt.Errorf("MPEG-1 Video streams with B-frames are not supported (yet)")
					}
					lastPTS = tunit.PTS

					return track.record(&sample{
						PartSample: &fmp4.PartSample{
							Payload:         tunit.Frame,
							IsNonSyncSample: !randomAccess,
						},
						dts: tunit.PTS,
					})
				})

			case *format.MJPEG:
				codec := &fmp4.CodecMJPEG{
					Width:  800,
					Height: 600,
				}
				track := addTrack(codec)

				parsed := false

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.MJPEG)
					if tunit.Frame == nil {
						return nil
					}

					if !parsed {
						parsed = true
						width, height, err := jpegExtractSize(tunit.Frame)
						if err != nil {
							return err
						}
						codec.Width = width
						codec.Height = height
						updateCodecs()
					}

					return track.record(&sample{
						PartSample: &fmp4.PartSample{
							Payload: tunit.Frame,
						},
						dts: tunit.PTS,
					})
				})

			case *format.Opus:
				codec := &fmp4.CodecOpus{
					ChannelCount: func() int {
						if forma.IsStereo {
							return 2
						}
						return 1
					}(),
				}
				track := addTrack(codec)

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.Opus)
					if tunit.Packets == nil {
						return nil
					}

					pts := tunit.PTS

					for _, packet := range tunit.Packets {
						err := track.record(&sample{
							PartSample: &fmp4.PartSample{
								Payload: packet,
							},
							dts: pts,
						})
						if err != nil {
							return err
						}

						pts += opus.PacketDuration(packet)
					}

					return nil
				})

			case *format.MPEG4Audio:
				codec := &fmp4.CodecMPEG4Audio{
					Config: *forma.GetConfig(),
				}
				track := addTrack(codec)

				sampleRate := time.Duration(forma.ClockRate())

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.MPEG4Audio)
					if tunit.AUs == nil {
						return nil
					}

					for i, au := range tunit.AUs {
						auPTS := tunit.PTS + time.Duration(i)*mpeg4audio.SamplesPerAccessUnit*
							time.Second/sampleRate

						err := track.record(&sample{
							PartSample: &fmp4.PartSample{
								Payload: au,
							},
							dts: auPTS,
						})
						if err != nil {
							return err
						}
					}

					return nil
				})

			case *format.MPEG1Audio:
				codec := &fmp4.CodecMPEG1Audio{
					SampleRate:   32000,
					ChannelCount: 2,
				}
				track := addTrack(codec)

				parsed := false

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.MPEG1Audio)
					if tunit.Frames == nil {
						return nil
					}

					pts := tunit.PTS

					for _, frame := range tunit.Frames {
						var h mpeg1audio.FrameHeader
						err := h.Unmarshal(frame)
						if err != nil {
							return err
						}

						if !parsed {
							parsed = true
							codec.SampleRate = h.SampleRate
							codec.ChannelCount = mpeg1audioChannelCount(h.ChannelMode)
							updateCodecs()
						}

						err = track.record(&sample{
							PartSample: &fmp4.PartSample{
								Payload: frame,
							},
							dts: pts,
						})
						if err != nil {
							return err
						}

						pts += time.Duration(h.SampleCount()) *
							time.Second / time.Duration(h.SampleRate)
					}

					return nil
				})

			case *format.AC3:
				codec := &fmp4.CodecAC3{
					SampleRate:   forma.SampleRate,
					ChannelCount: forma.ChannelCount,
					Fscod:        0,
					Bsid:         8,
					Bsmod:        0,
					Acmod:        7,
					LfeOn:        true,
					BitRateCode:  7,
				}
				track := addTrack(codec)

				parsed := false

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.AC3)
					if tunit.Frames == nil {
						return nil
					}

					pts := tunit.PTS

					for _, frame := range tunit.Frames {
						var syncInfo ac3.SyncInfo
						err := syncInfo.Unmarshal(frame)
						if err != nil {
							return fmt.Errorf("invalid AC-3 frame: %s", err)
						}

						var bsi ac3.BSI
						err = bsi.Unmarshal(frame[5:])
						if err != nil {
							return fmt.Errorf("invalid AC-3 frame: %s", err)
						}

						if !parsed {
							parsed = true
							codec.SampleRate = syncInfo.SampleRate()
							codec.ChannelCount = bsi.ChannelCount()
							codec.Fscod = syncInfo.Fscod
							codec.Bsid = bsi.Bsid
							codec.Bsmod = bsi.Bsmod
							codec.Acmod = bsi.Acmod
							codec.LfeOn = bsi.LfeOn
							codec.BitRateCode = syncInfo.Frmsizecod >> 1
							updateCodecs()
						}

						err = track.record(&sample{
							PartSample: &fmp4.PartSample{
								Payload: frame,
							},
							dts: pts,
						})
						if err != nil {
							return err
						}

						pts += time.Duration(ac3.SamplesPerFrame) *
							time.Second / time.Duration(codec.SampleRate)
					}

					return nil
				})

			case *format.G722:
				// TODO

			case *format.G711:
				// TODO

			case *format.LPCM:
				codec := &fmp4.CodecLPCM{
					LittleEndian: false,
					BitDepth:     forma.BitDepth,
					SampleRate:   forma.SampleRate,
					ChannelCount: forma.ChannelCount,
				}
				track := addTrack(codec)

				f.a.wrapper.Stream.AddReader(f.a.writer, media, forma, func(u unit.Unit) error {
					tunit := u.(*unit.LPCM)
					if tunit.Samples == nil {
						return nil
					}

					return track.record(&sample{
						PartSample: &fmp4.PartSample{
							Payload: tunit.Samples,
						},
						dts: tunit.PTS,
					})
				})
			}
		}
	}

	f.a.wrapper.Log(logger.Info, "recording %d %s",
		len(f.tracks),
		func() string {
			if len(f.tracks) == 1 {
				return "track"
			}
			return "tracks"
		}())
}

func (f *recFormatFMP4) close() {
	if f.currentSegment != nil {
		f.currentSegment.close() //nolint:errcheck
	}
}
