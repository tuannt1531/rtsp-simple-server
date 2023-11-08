package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

type nilLogger struct{}

func (nilLogger) Log(_ logger.Level, _ string, _ ...interface{}) {
}

func TestAgent(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{
			Type: description.MediaTypeVideo,
			Formats: []format.Format{&format.H265{
				PayloadTyp: 96,
			}},
		},
		{
			Type: description.MediaTypeVideo,
			Formats: []format.Format{&format.H264{
				PayloadTyp:        96,
				PacketizationMode: 1,
			}},
		},
		{
			Type: description.MediaTypeAudio,
			Formats: []format.Format{&format.MPEG4Audio{
				PayloadTyp: 96,
				Config: &mpeg4audio.Config{
					Type:         2,
					SampleRate:   44100,
					ChannelCount: 2,
				},
				SizeLength:       13,
				IndexLength:      3,
				IndexDeltaLength: 3,
			}},
		},
	}}

	writeToStream := func(stream *stream.Stream) {
		for i := 0; i < 3; i++ {
			stream.WriteUnit(desc.Medias[0], desc.Medias[0].Formats[0], &unit.H265{
				Base: unit.Base{
					PTS: (50 + time.Duration(i)) * time.Second,
				},
				AU: [][]byte{
					{ // VPS
						0x40, 0x01, 0x0c, 0x01, 0xff, 0xff, 0x02, 0x20,
						0x00, 0x00, 0x03, 0x00, 0xb0, 0x00, 0x00, 0x03,
						0x00, 0x00, 0x03, 0x00, 0x7b, 0x18, 0xb0, 0x24,
					},
					{ // SPS
						0x42, 0x01, 0x01, 0x02, 0x20, 0x00, 0x00, 0x03,
						0x00, 0xb0, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03,
						0x00, 0x7b, 0xa0, 0x07, 0x82, 0x00, 0x88, 0x7d,
						0xb6, 0x71, 0x8b, 0x92, 0x44, 0x80, 0x53, 0x88,
						0x88, 0x92, 0xcf, 0x24, 0xa6, 0x92, 0x72, 0xc9,
						0x12, 0x49, 0x22, 0xdc, 0x91, 0xaa, 0x48, 0xfc,
						0xa2, 0x23, 0xff, 0x00, 0x01, 0x00, 0x01, 0x6a,
						0x02, 0x02, 0x02, 0x01,
					},
					{ // PPS
						0x44, 0x01, 0xc0, 0x25, 0x2f, 0x05, 0x32, 0x40,
					},
					{byte(h265.NALUType_CRA_NUT) << 1, 0}, // IDR
				},
			})

			stream.WriteUnit(desc.Medias[1], desc.Medias[1].Formats[0], &unit.H264{
				Base: unit.Base{
					PTS: (50 + time.Duration(i)) * time.Second,
				},
				AU: [][]byte{
					{ // SPS
						0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
						0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
						0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20,
					},
					{ // PPS
						0x08, 0x06, 0x07, 0x08,
					},
					{5}, // IDR
				},
			})

			stream.WriteUnit(desc.Medias[2], desc.Medias[2].Formats[0], &unit.MPEG4Audio{
				Base: unit.Base{
					PTS: (50 + time.Duration(i)) * time.Second,
				},
				AUs: [][]byte{{1, 2, 3, 4}},
			})
		}
	}

	for _, ca := range []string{"fmp4", "mpegts"} {
		t.Run(ca, func(t *testing.T) {
			n := 0
			timeNow = func() time.Time {
				n++
				switch n {
				case 1:
					return time.Date(2008, 0o5, 20, 22, 15, 25, 0, time.UTC)

				case 2:
					return time.Date(2009, 0o5, 20, 22, 15, 25, 0, time.UTC)

				case 3:
					return time.Date(2010, 0o5, 20, 22, 15, 25, 0, time.UTC)

				default:
					return time.Date(2011, 0o5, 20, 22, 15, 25, 0, time.UTC)
				}
			}

			stream, err := stream.New(
				1460,
				desc,
				true,
				&nilLogger{},
			)
			require.NoError(t, err)
			defer stream.Close()

			dir, err := os.MkdirTemp("", "mediamtx-agent")
			require.NoError(t, err)
			defer os.RemoveAll(dir)

			recordPath := filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f")

			segCreated := make(chan struct{}, 4)
			segDone := make(chan struct{}, 4)

			var f conf.RecordFormat
			if ca == "fmp4" {
				f = conf.RecordFormatFMP4
			} else {
				f = conf.RecordFormatMPEGTS
			}

			w := &Agent{
				WriteQueueSize:  1024,
				RecordPath:      recordPath,
				Format:          f,
				PartDuration:    100 * time.Millisecond,
				SegmentDuration: 1 * time.Second,
				PathName:        "mypath",
				Stream:          stream,
				OnSegmentCreate: func(fpath string) {
					segCreated <- struct{}{}
				},
				OnSegmentComplete: func(fpath string) {
					segDone <- struct{}{}
				},
				Parent:       &nilLogger{},
				restartPause: 1 * time.Millisecond,
			}
			w.Initialize()

			writeToStream(stream)

			// simulate a write error
			stream.WriteUnit(desc.Medias[1], desc.Medias[1].Formats[0], &unit.H264{
				Base: unit.Base{
					PTS: 0,
				},
				AU: [][]byte{
					{5}, // IDR
				},
			})

			for i := 0; i < 2; i++ {
				<-segCreated
				<-segDone
			}

			var ext string
			if ca == "fmp4" {
				ext = "mp4"
			} else {
				ext = "ts"
			}

			_, err = os.Stat(filepath.Join(dir, "mypath", "2008-05-20_22-15-25-000000."+ext))
			require.NoError(t, err)

			_, err = os.Stat(filepath.Join(dir, "mypath", "2009-05-20_22-15-25-000000."+ext))
			require.NoError(t, err)

			time.Sleep(50 * time.Millisecond)

			writeToStream(stream)

			time.Sleep(50 * time.Millisecond)

			w.Close()

			for i := 0; i < 2; i++ {
				<-segCreated
				<-segDone
			}

			_, err = os.Stat(filepath.Join(dir, "mypath", "2010-05-20_22-15-25-000000."+ext))
			require.NoError(t, err)

			_, err = os.Stat(filepath.Join(dir, "mypath", "2011-05-20_22-15-25-000000."+ext))
			require.NoError(t, err)
		})
	}
}
