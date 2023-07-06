package core

import (
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
)

func metric(key string, tags string, value int64) string {
	return key + tags + " " + strconv.FormatInt(value, 10) + "\n"
}

type metricsParent interface {
	logger.Writer
}

type metrics struct {
	parent metricsParent

	httpServer    *httpServer
	mutex         sync.Mutex
	pathManager   apiPathManager
	rtspServer    apiRTSPServer
	rtspsServer   apiRTSPServer
	rtmpServer    apiRTMPServer
	hlsManager    apiHLSManager
	webRTCManager apiWebRTCManager
}

func newMetrics(
	address string,
	readTimeout conf.StringDuration,
	parent metricsParent,
) (*metrics, error) {
	m := &metrics{
		parent: parent,
	}

	router := gin.New()
	router.SetTrustedProxies(nil)

	mwLog := httpLoggerMiddleware(m)
	router.NoRoute(mwLog)
	router.GET("/metrics", mwLog, m.onMetrics)

	var err error
	m.httpServer, err = newHTTPServer(
		address,
		readTimeout,
		"",
		"",
		router,
	)
	if err != nil {
		return nil, err
	}

	m.Log(logger.Info, "listener opened on "+address)

	return m, nil
}

func (m *metrics) close() {
	m.Log(logger.Info, "listener is closing")
	m.httpServer.close()
}

func (m *metrics) Log(level logger.Level, format string, args ...interface{}) {
	m.parent.Log(level, "[metrics] "+format, args...)
}

func (m *metrics) onMetrics(ctx *gin.Context) {
	out := ""

	data, err := m.pathManager.apiPathsList()
	if err == nil && len(data.Items) != 0 {
		for _, i := range data.Items {
			var state string
			if i.SourceReady {
				state = "ready"
			} else {
				state = "notReady"
			}

			tags := "{name=\"" + i.Name + "\",state=\"" + state + "\"}"
			out += metric("paths", tags, 1)
			out += metric("paths_bytes_received", tags, int64(i.BytesReceived))
		}
	} else {
		out += metric("paths", "", 0)
	}

	if !interfaceIsEmpty(m.hlsManager) {
		data, err := m.hlsManager.apiMuxersList()
		if err == nil && len(data.Items) != 0 {
			for _, i := range data.Items {
				tags := "{name=\"" + i.Path + "\"}"
				out += metric("hls_muxers", tags, 1)
				out += metric("hls_muxers_bytes_sent", tags, int64(i.BytesSent))
			}
		} else {
			out += metric("hls_muxers", "", 0)
			out += metric("hls_muxers_bytes_sent", "", 0)
		}
	}

	if !interfaceIsEmpty(m.rtspServer) { //nolint:dupl
		func() {
			data, err := m.rtspServer.apiConnsList()
			if err == nil && len(data.Items) != 0 {
				for _, i := range data.Items {
					tags := "{id=\"" + i.ID.String() + "\"}"
					out += metric("rtsp_conns", tags, 1)
					out += metric("rtsp_conns_bytes_received", tags, int64(i.BytesReceived))
					out += metric("rtsp_conns_bytes_sent", tags, int64(i.BytesSent))
				}
			} else {
				out += metric("rtsp_conns", "", 0)
				out += metric("rtsp_conns_bytes_received", "", 0)
				out += metric("rtsp_conns_bytes_sent", "", 0)
			}
		}()

		func() {
			data, err := m.rtspServer.apiSessionsList()
			if err == nil && len(data.Items) != 0 {
				for _, i := range data.Items {
					tags := "{id=\"" + i.ID.String() + "\",state=\"" + i.State + "\"}"
					out += metric("rtsp_sessions", tags, 1)
					out += metric("rtsp_sessions_bytes_received", tags, int64(i.BytesReceived))
					out += metric("rtsp_sessions_bytes_sent", tags, int64(i.BytesSent))
				}
			} else {
				out += metric("rtsp_sessions", "", 0)
				out += metric("rtsp_sessions_bytes_received", "", 0)
				out += metric("rtsp_sessions_bytes_sent", "", 0)
			}
		}()
	}

	if !interfaceIsEmpty(m.rtspsServer) { //nolint:dupl
		func() {
			data, err := m.rtspsServer.apiConnsList()
			if err == nil && len(data.Items) != 0 {
				for _, i := range data.Items {
					tags := "{id=\"" + i.ID.String() + "\"}"
					out += metric("rtsps_conns", tags, 1)
					out += metric("rtsps_conns_bytes_received", tags, int64(i.BytesReceived))
					out += metric("rtsps_conns_bytes_sent", tags, int64(i.BytesSent))
				}
			} else {
				out += metric("rtsps_conns", "", 0)
				out += metric("rtsps_conns_bytes_received", "", 0)
				out += metric("rtsps_conns_bytes_sent", "", 0)
			}
		}()

		func() {
			data, err := m.rtspsServer.apiSessionsList()
			if err == nil && len(data.Items) != 0 {
				for _, i := range data.Items {
					tags := "{id=\"" + i.ID.String() + "\",state=\"" + i.State + "\"}"
					out += metric("rtsps_sessions", tags, 1)
					out += metric("rtsps_sessions_bytes_received", tags, int64(i.BytesReceived))
					out += metric("rtsps_sessions_bytes_sent", tags, int64(i.BytesSent))
				}
			} else {
				out += metric("rtsps_sessions", "", 0)
				out += metric("rtsps_sessions_bytes_received", "", 0)
				out += metric("rtsps_sessions_bytes_sent", "", 0)
			}
		}()
	}

	if !interfaceIsEmpty(m.rtmpServer) {
		data, err := m.rtmpServer.apiConnsList()
		if err == nil && len(data.Items) != 0 {
			for _, i := range data.Items {
				tags := "{id=\"" + i.ID.String() + "\",state=\"" + i.State + "\"}"
				out += metric("rtmp_conns", tags, 1)
				out += metric("rtmp_conns_bytes_received", tags, int64(i.BytesReceived))
				out += metric("rtmp_conns_bytes_sent", tags, int64(i.BytesSent))
			}
		} else {
			out += metric("rtmp_conns", "", 0)
			out += metric("rtmp_conns_bytes_received", "", 0)
			out += metric("rtmp_conns_bytes_sent", "", 0)
		}
	}

	if !interfaceIsEmpty(m.webRTCManager) {
		data, err := m.webRTCManager.apiSessionsList()
		if err == nil && len(data.Items) != 0 {
			for _, i := range data.Items {
				tags := "{id=\"" + i.ID.String() + "\"}"
				out += metric("webrtc_sessions", tags, 1)
				out += metric("webrtc_sessions_bytes_received", tags, int64(i.BytesReceived))
				out += metric("webrtc_sessions_bytes_sent", tags, int64(i.BytesSent))
			}
		} else {
			out += metric("webrtc_sessions", "", 0)
			out += metric("webrtc_sessions_bytes_received", "", 0)
			out += metric("webrtc_sessions_bytes_sent", "", 0)
		}
	}

	ctx.Writer.WriteHeader(http.StatusOK)
	io.WriteString(ctx.Writer, out)
}

// pathManagerSet is called by pathManager.
func (m *metrics) pathManagerSet(s apiPathManager) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.pathManager = s
}

// hlsManagerSet is called by hlsManager.
func (m *metrics) hlsManagerSet(s apiHLSManager) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.hlsManager = s
}

// rtspServerSet is called by rtspServer (plain).
func (m *metrics) rtspServerSet(s apiRTSPServer) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.rtspServer = s
}

// rtspsServerSet is called by rtspServer (tls).
func (m *metrics) rtspsServerSet(s apiRTSPServer) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.rtspsServer = s
}

// rtmpServerSet is called by rtmpServer.
func (m *metrics) rtmpServerSet(s apiRTMPServer) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.rtmpServer = s
}

// webRTCManagerSet is called by webRTCManager.
func (m *metrics) webRTCManagerSet(s apiWebRTCManager) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.webRTCManager = s
}
