package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg4audio"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/rtmp"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

const (
	rtmpPauseAfterAuthError = 2 * time.Second
)

func pathNameAndQuery(inURL *url.URL) (string, url.Values, string) {
	// remove leading and trailing slashes inserted by OBS and some other clients
	tmp := strings.TrimRight(inURL.String(), "/")
	ur, _ := url.Parse(tmp)
	pathName := strings.TrimLeft(ur.Path, "/")
	return pathName, ur.Query(), ur.RawQuery
}

type rtmpConnState int

const (
	rtmpConnStateRead rtmpConnState = iota + 1
	rtmpConnStatePublish
)

type rtmpConnPathManager interface {
	addReader(req pathAddReaderReq) pathAddReaderRes
	addPublisher(req pathAddPublisherReq) pathAddPublisherRes
}

type rtmpConnParent interface {
	logger.Writer
	closeConn(*rtmpConn)
}

type rtmpConn struct {
	isTLS               bool
	rtspAddress         string
	readTimeout         conf.StringDuration
	writeTimeout        conf.StringDuration
	writeQueueSize      int
	runOnConnect        string
	runOnConnectRestart bool
	wg                  *sync.WaitGroup
	nconn               net.Conn
	externalCmdPool     *externalcmd.Pool
	pathManager         rtmpConnPathManager
	parent              rtmpConnParent

	ctx       context.Context
	ctxCancel func()
	uuid      uuid.UUID
	created   time.Time
	mutex     sync.RWMutex
	conn      *rtmp.Conn
	state     rtmpConnState
	pathName  string
}

func newRTMPConn(
	parentCtx context.Context,
	isTLS bool,
	rtspAddress string,
	readTimeout conf.StringDuration,
	writeTimeout conf.StringDuration,
	writeQueueSize int,
	runOnConnect string,
	runOnConnectRestart bool,
	wg *sync.WaitGroup,
	nconn net.Conn,
	externalCmdPool *externalcmd.Pool,
	pathManager rtmpConnPathManager,
	parent rtmpConnParent,
) *rtmpConn {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	c := &rtmpConn{
		isTLS:               isTLS,
		rtspAddress:         rtspAddress,
		readTimeout:         readTimeout,
		writeTimeout:        writeTimeout,
		writeQueueSize:      writeQueueSize,
		runOnConnect:        runOnConnect,
		runOnConnectRestart: runOnConnectRestart,
		wg:                  wg,
		nconn:               nconn,
		externalCmdPool:     externalCmdPool,
		pathManager:         pathManager,
		parent:              parent,
		ctx:                 ctx,
		ctxCancel:           ctxCancel,
		uuid:                uuid.New(),
		created:             time.Now(),
	}

	c.Log(logger.Info, "opened")

	c.wg.Add(1)
	go c.run()

	return c
}

func (c *rtmpConn) close() {
	c.ctxCancel()
}

func (c *rtmpConn) remoteAddr() net.Addr {
	return c.nconn.RemoteAddr()
}

func (c *rtmpConn) Log(level logger.Level, format string, args ...interface{}) {
	c.parent.Log(level, "[conn %v] "+format, append([]interface{}{c.nconn.RemoteAddr()}, args...)...)
}

func (c *rtmpConn) ip() net.IP {
	return c.nconn.RemoteAddr().(*net.TCPAddr).IP
}

func (c *rtmpConn) run() {
	defer c.wg.Done()

	if c.runOnConnect != "" {
		c.Log(logger.Info, "runOnConnect command started")
		_, port, _ := net.SplitHostPort(c.rtspAddress)
		onConnectCmd := externalcmd.NewCmd(
			c.externalCmdPool,
			c.runOnConnect,
			c.runOnConnectRestart,
			externalcmd.Environment{
				"MTX_PATH":  "",
				"RTSP_PATH": "", // deprecated
				"RTSP_PORT": port,
			},
			func(err error) {
				c.Log(logger.Info, "runOnConnect command exited: %v", err)
			})

		defer func() {
			onConnectCmd.Close()
			c.Log(logger.Info, "runOnConnect command stopped")
		}()
	}

	err := c.runInner()

	c.ctxCancel()

	c.parent.closeConn(c)

	c.Log(logger.Info, "closed (%v)", err)
}

func (c *rtmpConn) runInner() error {
	readerErr := make(chan error)
	go func() {
		readerErr <- c.runReader()
	}()

	select {
	case err := <-readerErr:
		c.nconn.Close()
		return err

	case <-c.ctx.Done():
		c.nconn.Close()
		<-readerErr
		return errors.New("terminated")
	}
}

func (c *rtmpConn) runReader() error {
	c.nconn.SetReadDeadline(time.Now().Add(time.Duration(c.readTimeout)))
	c.nconn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
	conn, u, publish, err := rtmp.NewServerConn(c.nconn)
	if err != nil {
		return err
	}

	c.mutex.Lock()
	c.conn = conn
	c.mutex.Unlock()

	if !publish {
		return c.runRead(conn, u)
	}
	return c.runPublish(conn, u)
}

func (c *rtmpConn) runRead(conn *rtmp.Conn, u *url.URL) error {
	pathName, query, rawQuery := pathNameAndQuery(u)

	res := c.pathManager.addReader(pathAddReaderReq{
		author:   c,
		pathName: pathName,
		credentials: authCredentials{
			query: rawQuery,
			ip:    c.ip(),
			user:  query.Get("user"),
			pass:  query.Get("pass"),
			proto: authProtocolRTMP,
			id:    &c.uuid,
		},
	})

	if res.err != nil {
		if terr, ok := res.err.(*errAuthentication); ok {
			// wait some seconds to stop brute force attacks
			<-time.After(rtmpPauseAfterAuthError)
			return terr
		}
		return res.err
	}

	defer res.path.removeReader(pathRemoveReaderReq{author: c})

	c.mutex.Lock()
	c.state = rtmpConnStateRead
	c.pathName = pathName
	c.mutex.Unlock()

	writer := newAsyncWriter(c.writeQueueSize, c)

	var medias []*description.Media
	var w *rtmp.Writer

	videoMedia, videoFormat := c.setupVideo(
		&w,
		res.stream,
		writer)
	if videoMedia != nil {
		medias = append(medias, videoMedia)
	}

	audioMedia, audioFormat := c.setupAudio(
		&w,
		res.stream,
		writer)
	if audioFormat != nil {
		medias = append(medias, audioMedia)
	}

	if videoFormat == nil && audioFormat == nil {
		return fmt.Errorf(
			"the stream doesn't contain any supported codec, which are currently H264, MPEG-4 Audio, MPEG-1/2 Audio")
	}

	defer res.stream.RemoveReader(c)

	c.Log(logger.Info, "is reading from path '%s', %s",
		res.path.name, sourceMediaInfo(medias))

	pathConf := res.path.safeConf()

	if pathConf.RunOnRead != "" {
		c.Log(logger.Info, "runOnRead command started")
		onReadCmd := externalcmd.NewCmd(
			c.externalCmdPool,
			pathConf.RunOnRead,
			pathConf.RunOnReadRestart,
			res.path.externalCmdEnv(),
			func(err error) {
				c.Log(logger.Info, "runOnRead command exited: %v", err)
			})
		defer func() {
			onReadCmd.Close()
			c.Log(logger.Info, "runOnRead command stopped")
		}()
	}

	var err error
	w, err = rtmp.NewWriter(conn, videoFormat, audioFormat)
	if err != nil {
		return err
	}

	// disable read deadline
	c.nconn.SetReadDeadline(time.Time{})

	writer.start()

	select {
	case <-c.ctx.Done():
		writer.stop()
		return fmt.Errorf("terminated")

	case err := <-writer.error():
		return err
	}
}

func (c *rtmpConn) setupVideo(
	w **rtmp.Writer,
	stream *stream.Stream,
	writer *asyncWriter,
) (*description.Media, format.Format) {
	var videoFormatH264 *format.H264
	videoMedia := stream.Desc().FindFormat(&videoFormatH264)

	if videoFormatH264 != nil {
		var videoDTSExtractor *h264.DTSExtractor

		stream.AddReader(c, videoMedia, videoFormatH264, func(u unit.Unit) {
			writer.push(func() error {
				tunit := u.(*unit.H264)

				if tunit.AU == nil {
					return nil
				}

				pts := tunit.PTS

				idrPresent := false
				nonIDRPresent := false

				for _, nalu := range tunit.AU {
					typ := h264.NALUType(nalu[0] & 0x1F)
					switch typ {
					case h264.NALUTypeIDR:
						idrPresent = true

					case h264.NALUTypeNonIDR:
						nonIDRPresent = true
					}
				}

				var dts time.Duration

				// wait until we receive an IDR
				if videoDTSExtractor == nil {
					if !idrPresent {
						return nil
					}

					videoDTSExtractor = h264.NewDTSExtractor()

					var err error
					dts, err = videoDTSExtractor.Extract(tunit.AU, pts)
					if err != nil {
						return err
					}
				} else {
					if !idrPresent && !nonIDRPresent {
						return nil
					}

					var err error
					dts, err = videoDTSExtractor.Extract(tunit.AU, pts)
					if err != nil {
						return err
					}
				}

				c.nconn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
				return (*w).WriteH264(pts, dts, idrPresent, tunit.AU)
			})
		})

		return videoMedia, videoFormatH264
	}

	return nil, nil
}

func (c *rtmpConn) setupAudio(
	w **rtmp.Writer,
	stream *stream.Stream,
	writer *asyncWriter,
) (*description.Media, format.Format) {
	var audioFormatMPEG4Generic *format.MPEG4AudioGeneric
	audioMedia := stream.Desc().FindFormat(&audioFormatMPEG4Generic)

	if audioMedia != nil {
		stream.AddReader(c, audioMedia, audioFormatMPEG4Generic, func(u unit.Unit) {
			writer.push(func() error {
				tunit := u.(*unit.MPEG4AudioGeneric)

				if tunit.AUs == nil {
					return nil
				}

				pts := tunit.PTS

				for i, au := range tunit.AUs {
					c.nconn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
					err := (*w).WriteMPEG4Audio(
						pts+time.Duration(i)*mpeg4audio.SamplesPerAccessUnit*
							time.Second/time.Duration(audioFormatMPEG4Generic.ClockRate()),
						au,
					)
					if err != nil {
						return err
					}
				}

				return nil
			})
		})

		return audioMedia, audioFormatMPEG4Generic
	}

	var audioFormatMPEG4AudioLATM *format.MPEG4AudioLATM
	audioMedia = stream.Desc().FindFormat(&audioFormatMPEG4AudioLATM)

	if audioMedia != nil &&
		audioFormatMPEG4AudioLATM.Config != nil &&
		len(audioFormatMPEG4AudioLATM.Config.Programs) == 1 &&
		len(audioFormatMPEG4AudioLATM.Config.Programs[0].Layers) == 1 {
		stream.AddReader(c, audioMedia, audioFormatMPEG4AudioLATM, func(u unit.Unit) {
			writer.push(func() error {
				tunit := u.(*unit.MPEG4AudioLATM)

				if tunit.AU == nil {
					return nil
				}

				pts := tunit.PTS

				c.nconn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
				return (*w).WriteMPEG4Audio(pts, tunit.AU)
			})
		})

		return audioMedia, audioFormatMPEG4AudioLATM
	}

	var audioFormatMPEG1 *format.MPEG1Audio
	audioMedia = stream.Desc().FindFormat(&audioFormatMPEG1)

	if audioMedia != nil {
		stream.AddReader(c, audioMedia, audioFormatMPEG1, func(u unit.Unit) {
			writer.push(func() error {
				tunit := u.(*unit.MPEG1Audio)

				pts := tunit.PTS

				for _, frame := range tunit.Frames {
					var h mpeg1audio.FrameHeader
					err := h.Unmarshal(frame)
					if err != nil {
						return err
					}

					if !(!h.MPEG2 && h.Layer == 3) {
						return fmt.Errorf("RTMP only supports MPEG-1 layer 3 audio")
					}

					c.nconn.SetWriteDeadline(time.Now().Add(time.Duration(c.writeTimeout)))
					err = (*w).WriteMPEG1Audio(pts, &h, frame)
					if err != nil {
						return err
					}

					pts += time.Duration(h.SampleCount()) *
						time.Second / time.Duration(h.SampleRate)
				}

				return nil
			})
		})

		return audioMedia, audioFormatMPEG1
	}

	return nil, nil
}

func (c *rtmpConn) runPublish(conn *rtmp.Conn, u *url.URL) error {
	pathName, query, rawQuery := pathNameAndQuery(u)

	res := c.pathManager.addPublisher(pathAddPublisherReq{
		author:   c,
		pathName: pathName,
		credentials: authCredentials{
			query: rawQuery,
			ip:    c.ip(),
			user:  query.Get("user"),
			pass:  query.Get("pass"),
			proto: authProtocolRTMP,
			id:    &c.uuid,
		},
	})

	if res.err != nil {
		if terr, ok := res.err.(*errAuthentication); ok {
			// wait some seconds to stop brute force attacks
			<-time.After(rtmpPauseAfterAuthError)
			return terr
		}
		return res.err
	}

	defer res.path.removePublisher(pathRemovePublisherReq{author: c})

	c.mutex.Lock()
	c.state = rtmpConnStatePublish
	c.pathName = pathName
	c.mutex.Unlock()

	r, err := rtmp.NewReader(conn)
	if err != nil {
		return err
	}
	videoFormat, audioFormat := r.Tracks()

	var medias []*description.Media
	var stream *stream.Stream

	if videoFormat != nil {
		videoMedia := &description.Media{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{videoFormat},
		}
		medias = append(medias, videoMedia)

		switch videoFormat.(type) {
		case *format.AV1:
			r.OnDataAV1(func(pts time.Duration, tu [][]byte) {
				stream.WriteUnit(videoMedia, videoFormat, &unit.AV1{
					Base: unit.Base{
						NTP: time.Now(),
						PTS: pts,
					},
					TU: tu,
				})
			})

		case *format.VP9:
			r.OnDataVP9(func(pts time.Duration, frame []byte) {
				stream.WriteUnit(videoMedia, videoFormat, &unit.VP9{
					Base: unit.Base{
						NTP: time.Now(),
						PTS: pts,
					},
					Frame: frame,
				})
			})

		case *format.H265:
			r.OnDataH265(func(pts time.Duration, au [][]byte) {
				stream.WriteUnit(videoMedia, videoFormat, &unit.H265{
					Base: unit.Base{
						NTP: time.Now(),
						PTS: pts,
					},
					AU: au,
				})
			})

		case *format.H264:
			r.OnDataH264(func(pts time.Duration, au [][]byte) {
				stream.WriteUnit(videoMedia, videoFormat, &unit.H264{
					Base: unit.Base{
						NTP: time.Now(),
						PTS: pts,
					},
					AU: au,
				})
			})

		default:
			return fmt.Errorf("unsupported video codec: %T", videoFormat)
		}
	}

	if audioFormat != nil { //nolint:dupl
		audioMedia := &description.Media{
			Type:    description.MediaTypeAudio,
			Formats: []format.Format{audioFormat},
		}
		medias = append(medias, audioMedia)

		switch audioFormat.(type) {
		case *format.MPEG4AudioGeneric:
			r.OnDataMPEG4Audio(func(pts time.Duration, au []byte) {
				stream.WriteUnit(audioMedia, audioFormat, &unit.MPEG4AudioGeneric{
					Base: unit.Base{
						NTP: time.Now(),
						PTS: pts,
					},
					AUs: [][]byte{au},
				})
			})

		case *format.MPEG1Audio:
			r.OnDataMPEG1Audio(func(pts time.Duration, frame []byte) {
				stream.WriteUnit(audioMedia, audioFormat, &unit.MPEG1Audio{
					Base: unit.Base{
						NTP: time.Now(),
						PTS: pts,
					},
					Frames: [][]byte{frame},
				})
			})

		default:
			return fmt.Errorf("unsupported audio codec: %T", audioFormat)
		}
	}

	rres := res.path.startPublisher(pathStartPublisherReq{
		author:             c,
		desc:               &description.Session{Medias: medias},
		generateRTPPackets: true,
	})
	if rres.err != nil {
		return rres.err
	}

	stream = rres.stream

	// disable write deadline to allow outgoing acknowledges
	c.nconn.SetWriteDeadline(time.Time{})

	for {
		c.nconn.SetReadDeadline(time.Now().Add(time.Duration(c.readTimeout)))
		err := r.Read()
		if err != nil {
			return err
		}
	}
}

// apiReaderDescribe implements reader.
func (c *rtmpConn) apiReaderDescribe() pathAPISourceOrReader {
	return pathAPISourceOrReader{
		Type: func() string {
			if c.isTLS {
				return "rtmpsConn"
			}
			return "rtmpConn"
		}(),
		ID: c.uuid.String(),
	}
}

// apiSourceDescribe implements source.
func (c *rtmpConn) apiSourceDescribe() pathAPISourceOrReader {
	return c.apiReaderDescribe()
}

func (c *rtmpConn) apiItem() *apiRTMPConn {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	bytesReceived := uint64(0)
	bytesSent := uint64(0)

	if c.conn != nil {
		bytesReceived = c.conn.BytesReceived()
		bytesSent = c.conn.BytesSent()
	}

	return &apiRTMPConn{
		ID:         c.uuid,
		Created:    c.created,
		RemoteAddr: c.remoteAddr().String(),
		State: func() apiRTMPConnState {
			switch c.state {
			case rtmpConnStateRead:
				return apiRTMPConnStateRead

			case rtmpConnStatePublish:
				return apiRTMPConnStatePublish

			default:
				return apiRTMPConnStateIdle
			}
		}(),
		Path:          c.pathName,
		BytesReceived: bytesReceived,
		BytesSent:     bytesSent,
	}
}
