package srt

import (
	"bufio"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/pkg/formats/mpegts"
	"github.com/datarhei/gosrt"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/staticsources/tester"
)

func TestSource(t *testing.T) {
	ln, err := srt.Listen("srt", "localhost:9002", srt.DefaultConfig())
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		conn, _, err := ln.Accept(func(req srt.ConnRequest) srt.ConnType {
			require.Equal(t, "sidname", req.StreamId())
			err := req.SetPassphrase("ttest1234567")
			if err != nil {
				return srt.REJECT
			}
			return srt.SUBSCRIBE
		})
		require.NoError(t, err)
		require.NotNil(t, conn)
		defer conn.Close()

		track := &mpegts.Track{
			Codec: &mpegts.CodecH264{},
		}

		bw := bufio.NewWriter(conn)
		w := mpegts.NewWriter(bw, []*mpegts.Track{track})
		require.NoError(t, err)

		err = w.WriteH26x(track, 0, 0, true, [][]byte{{ // IDR
			5, 1,
		}})
		require.NoError(t, err)

		err = w.WriteH26x(track, 0, 0, true, [][]byte{{ // non-IDR
			5, 2,
		}})
		require.NoError(t, err)

		err = bw.Flush()
		require.NoError(t, err)

		time.Sleep(1000 * time.Millisecond)
	}()

	te := tester.New(
		func(p defs.StaticSourceParent) defs.StaticSource {
			return &Source{
				ReadTimeout: conf.StringDuration(10 * time.Second),
				Parent:      p,
			}
		},
		&conf.Path{
			Source: "srt://localhost:9002?streamid=sidname&passphrase=ttest1234567",
		},
	)
	defer te.Close()

	<-te.Unit
}
