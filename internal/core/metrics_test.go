package core

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/mediacommon/pkg/formats/mpegts"
	"github.com/datarhei/gosrt"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/protocols/rtmp"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
)

func TestMetrics(t *testing.T) {
	serverCertFpath, err := writeTempFile(serverCert)
	require.NoError(t, err)
	defer os.Remove(serverCertFpath)

	serverKeyFpath, err := writeTempFile(serverKey)
	require.NoError(t, err)
	defer os.Remove(serverKeyFpath)

	p, ok := newInstance("hlsAlwaysRemux: yes\n" +
		"metrics: yes\n" +
		"webrtcServerCert: " + serverCertFpath + "\n" +
		"webrtcServerKey: " + serverKeyFpath + "\n" +
		"encryption: optional\n" +
		"serverCert: " + serverCertFpath + "\n" +
		"serverKey: " + serverKeyFpath + "\n" +
		"paths:\n" +
		"  all_others:\n")
	require.Equal(t, true, ok)
	defer p.Close()

	hc := &http.Client{Transport: &http.Transport{}}

	bo := httpPullFile(t, hc, "http://localhost:9998/metrics")

	require.Equal(t, `paths 0
hls_muxers 0
hls_muxers_bytes_sent 0
rtsp_conns 0
rtsp_conns_bytes_received 0
rtsp_conns_bytes_sent 0
rtsp_sessions 0
rtsp_sessions_bytes_received 0
rtsp_sessions_bytes_sent 0
rtsps_conns 0
rtsps_conns_bytes_received 0
rtsps_conns_bytes_sent 0
rtsps_sessions 0
rtsps_sessions_bytes_received 0
rtsps_sessions_bytes_sent 0
rtmp_conns 0
rtmp_conns_bytes_received 0
rtmp_conns_bytes_sent 0
srt_conns 0
srt_conns_bytes_received 0
srt_conns_bytes_sent 0
webrtc_sessions 0
webrtc_sessions_bytes_received 0
webrtc_sessions_bytes_sent 0
`, string(bo))

	terminate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(5)

	go func() {
		defer wg.Done()
		source := gortsplib.Client{}
		err := source.StartRecording("rtsp://localhost:8554/rtsp_path",
			&description.Session{Medias: []*description.Media{{
				Type:    description.MediaTypeVideo,
				Formats: []format.Format{testFormatH264},
			}}})
		require.NoError(t, err)
		defer source.Close()
		<-terminate
	}()

	go func() {
		defer wg.Done()
		source2 := gortsplib.Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
		err := source2.StartRecording("rtsps://localhost:8322/rtsps_path",
			&description.Session{Medias: []*description.Media{{
				Type:    description.MediaTypeVideo,
				Formats: []format.Format{testFormatH264},
			}}})
		require.NoError(t, err)
		defer source2.Close()
		<-terminate
	}()

	go func() {
		defer wg.Done()
		u, err := url.Parse("rtmp://localhost:1935/rtmp_path")
		require.NoError(t, err)

		nconn, err := net.Dial("tcp", u.Host)
		require.NoError(t, err)
		defer nconn.Close()

		conn, err := rtmp.NewClientConn(nconn, u, true)
		require.NoError(t, err)

		_, err = rtmp.NewWriter(conn, testFormatH264, nil)
		require.NoError(t, err)
		<-terminate
	}()

	go func() {
		defer wg.Done()

		su, err := url.Parse("http://localhost:8889/webrtc_path/whip")
		require.NoError(t, err)

		s := &webrtc.WHIPClient{
			HTTPClient: &http.Client{Transport: &http.Transport{}},
			URL:        su,
		}

		tracks, err := s.Publish(context.Background(), testMediaH264.Formats[0], nil)
		require.NoError(t, err)
		defer checkClose(t, s.Close)

		err = tracks[0].WriteRTP(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         true,
				PayloadType:    96,
				SequenceNumber: 123,
				Timestamp:      45343,
				SSRC:           563423,
			},
			Payload: []byte{1},
		})
		require.NoError(t, err)
		<-terminate
	}()

	go func() {
		defer wg.Done()

		srtConf := srt.DefaultConfig()
		address, err := srtConf.UnmarshalURL("srt://localhost:8890?streamid=publish:srt_path")
		require.NoError(t, err)

		err = srtConf.Validate()
		require.NoError(t, err)

		publisher, err := srt.Dial("srt", address, srtConf)
		require.NoError(t, err)
		defer publisher.Close()

		track := &mpegts.Track{
			Codec: &mpegts.CodecH264{},
		}

		bw := bufio.NewWriter(publisher)
		w := mpegts.NewWriter(bw, []*mpegts.Track{track})
		require.NoError(t, err)

		err = w.WriteH26x(track, 0, 0, true, [][]byte{
			{ // SPS
				0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
				0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
				0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9,
				0x20,
			},
			{ // PPS
				0x08, 0x06, 0x07, 0x08,
			},
			{ // IDR
				0x05, 1,
			},
		})
		require.NoError(t, err)

		err = bw.Flush()
		require.NoError(t, err)
		<-terminate
	}()

	time.Sleep(500 * time.Millisecond)

	bo = httpPullFile(t, hc, "http://localhost:9998/metrics")

	require.Regexp(t,
		`^paths\{name=".*?",state="ready"\} 1`+"\n"+
			`paths_bytes_received\{name=".*?",state="ready"\} [0-9]+`+"\n"+
			`paths_bytes_sent\{name=".*?",state="ready"\} 0`+"\n"+
			`paths\{name=".*?",state="ready"\} 1`+"\n"+
			`paths_bytes_received\{name=".*?",state="ready"\} [0-9]+`+"\n"+
			`paths_bytes_sent\{name=".*?",state="ready"\} 0`+"\n"+
			`paths\{name=".*?",state="ready"\} 1`+"\n"+
			`paths_bytes_received\{name=".*?",state="ready"\} [0-9]+`+"\n"+
			`paths_bytes_sent\{name=".*?",state="ready"\} 0`+"\n"+
			`paths\{name=".*?",state="ready"\} 1`+"\n"+
			`paths_bytes_received\{name=".*?",state="ready"\} [0-9]+`+"\n"+
			`paths_bytes_sent\{name=".*?",state="ready"\} 0`+"\n"+
			`paths\{name=".*?",state="ready"\} 1`+"\n"+
			`paths_bytes_received\{name=".*?",state="ready"\} [0-9]+`+"\n"+
			`paths_bytes_sent\{name=".*?",state="ready"\} 0`+"\n"+
			`hls_muxers\{name=".*?"\} 1`+"\n"+
			`hls_muxers_bytes_sent\{name=".*?"\} [0-9]+`+"\n"+
			`hls_muxers\{name=".*?"\} 1`+"\n"+
			`hls_muxers_bytes_sent\{name=".*?"\} [0-9]+`+"\n"+
			`hls_muxers\{name=".*?"\} 1`+"\n"+
			`hls_muxers_bytes_sent\{name=".*?"\} 0`+"\n"+
			`hls_muxers\{name=".*?"\} 1`+"\n"+
			`hls_muxers_bytes_sent\{name=".*?"\} 0`+"\n"+
			`hls_muxers\{name=".*?"\} 1`+"\n"+
			`hls_muxers_bytes_sent\{name=".*?"\} 0`+"\n"+
			`rtsp_conns\{id=".*?"\} 1`+"\n"+
			`rtsp_conns_bytes_received\{id=".*?"\} [0-9]+`+"\n"+
			`rtsp_conns_bytes_sent\{id=".*?"\} [0-9]+`+"\n"+
			`rtsp_sessions\{id=".*?",state="publish"\} 1`+"\n"+
			`rtsp_sessions_bytes_received\{id=".*?",state="publish"\} 0`+"\n"+
			`rtsp_sessions_bytes_sent\{id=".*?",state="publish"\} [0-9]+`+"\n"+
			`rtsps_conns\{id=".*?"\} 1`+"\n"+
			`rtsps_conns_bytes_received\{id=".*?"\} [0-9]+`+"\n"+
			`rtsps_conns_bytes_sent\{id=".*?"\} [0-9]+`+"\n"+
			`rtsps_sessions\{id=".*?",state="publish"\} 1`+"\n"+
			`rtsps_sessions_bytes_received\{id=".*?",state="publish"\} 0`+"\n"+
			`rtsps_sessions_bytes_sent\{id=".*?",state="publish"\} [0-9]+`+"\n"+
			`rtmp_conns\{id=".*?",state="publish"\} 1`+"\n"+
			`rtmp_conns_bytes_received\{id=".*?",state="publish"\} [0-9]+`+"\n"+
			`rtmp_conns_bytes_sent\{id=".*?",state="publish"\} [0-9]+`+"\n"+
			`srt_conns\{id=".*?",state="publish"\} 1`+"\n"+
			`srt_conns_bytes_received\{id=".*?",state="publish"\} [0-9]+`+"\n"+
			`srt_conns_bytes_sent\{id=".*?",state="publish"\} 0`+"\n"+
			`webrtc_sessions\{id=".*?",state="publish"\} 1`+"\n"+
			`webrtc_sessions_bytes_received\{id=".*?",state="publish"\} [0-9]+`+"\n"+
			`webrtc_sessions_bytes_sent\{id=".*?",state="publish"\} [0-9]+`+"\n"+
			"$",
		string(bo))

	close(terminate)
	wg.Wait()
}
