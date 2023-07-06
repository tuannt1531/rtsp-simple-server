package core

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v3/pkg/media"
	"github.com/bluenviron/gortsplib/v3/pkg/url"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
)

func newEmptyTimer() *time.Timer {
	t := time.NewTimer(0)
	<-t.C
	return t
}

type pathErrAuth struct {
	wrapped error
}

// Error implements the error interface.
func (e pathErrAuth) Error() string {
	return "authentication error"
}

type pathErrNoOnePublishing struct {
	pathName string
}

// Error implements the error interface.
func (e pathErrNoOnePublishing) Error() string {
	return fmt.Sprintf("no one is publishing to path '%s'", e.pathName)
}

type pathParent interface {
	logger.Writer
	pathSourceReady(*path)
	pathSourceNotReady(*path)
	onPathClose(*path)
}

type pathOnDemandState int

const (
	pathOnDemandStateInitial pathOnDemandState = iota
	pathOnDemandStateWaitingReady
	pathOnDemandStateReady
	pathOnDemandStateClosing
)

type pathSourceStaticSetReadyRes struct {
	stream *stream
	err    error
}

type pathSourceStaticSetReadyReq struct {
	medias             media.Medias
	generateRTPPackets bool
	res                chan pathSourceStaticSetReadyRes
}

type pathSourceStaticSetNotReadyReq struct {
	res chan struct{}
}

type pathReaderRemoveReq struct {
	author reader
	res    chan struct{}
}

type pathPublisherRemoveReq struct {
	author publisher
	res    chan struct{}
}

type pathGetPathConfRes struct {
	conf *conf.PathConf
	err  error
}

type pathGetPathConfReq struct {
	name        string
	publish     bool
	credentials authCredentials
	res         chan pathGetPathConfRes
}

type pathDescribeRes struct {
	path     *path
	stream   *stream
	redirect string
	err      error
}

type pathDescribeReq struct {
	pathName    string
	url         *url.URL
	credentials authCredentials
	res         chan pathDescribeRes
}

type pathReaderSetupPlayRes struct {
	path   *path
	stream *stream
	err    error
}

type pathReaderAddReq struct {
	author      reader
	pathName    string
	skipAuth    bool
	credentials authCredentials
	res         chan pathReaderSetupPlayRes
}

type pathPublisherAnnounceRes struct {
	path *path
	err  error
}

type pathPublisherAddReq struct {
	author      publisher
	pathName    string
	skipAuth    bool
	credentials authCredentials
	res         chan pathPublisherAnnounceRes
}

type pathPublisherRecordRes struct {
	stream *stream
	err    error
}

type pathPublisherStartReq struct {
	author             publisher
	medias             media.Medias
	generateRTPPackets bool
	res                chan pathPublisherRecordRes
}

type pathPublisherStopReq struct {
	author publisher
	res    chan struct{}
}

type pathAPISourceOrReader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type pathAPIPathsListRes struct {
	data  *apiPathsList
	paths map[string]*path
}

type pathAPIPathsListReq struct {
	res chan pathAPIPathsListRes
}

type pathAPIPathsGetRes struct {
	path *path
	data *apiPath
	err  error
}

type pathAPIPathsGetReq struct {
	name string
	res  chan pathAPIPathsGetRes
}

type path struct {
	rtspAddress       string
	readTimeout       conf.StringDuration
	writeTimeout      conf.StringDuration
	readBufferCount   int
	udpMaxPayloadSize int
	confName          string
	conf              *conf.PathConf
	name              string
	matches           []string
	wg                *sync.WaitGroup
	externalCmdPool   *externalcmd.Pool
	parent            pathParent

	ctx                            context.Context
	ctxCancel                      func()
	confMutex                      sync.RWMutex
	source                         source
	bytesReceived                  *uint64
	stream                         *stream
	readers                        map[reader]struct{}
	describeRequestsOnHold         []pathDescribeReq
	readerAddRequestsOnHold        []pathReaderAddReq
	onDemandCmd                    *externalcmd.Cmd
	onReadyCmd                     *externalcmd.Cmd
	onDemandStaticSourceState      pathOnDemandState
	onDemandStaticSourceReadyTimer *time.Timer
	onDemandStaticSourceCloseTimer *time.Timer
	onDemandPublisherState         pathOnDemandState
	onDemandPublisherReadyTimer    *time.Timer
	onDemandPublisherCloseTimer    *time.Timer

	// in
	chReloadConf              chan *conf.PathConf
	chSourceStaticSetReady    chan pathSourceStaticSetReadyReq
	chSourceStaticSetNotReady chan pathSourceStaticSetNotReadyReq
	chDescribe                chan pathDescribeReq
	chPublisherRemove         chan pathPublisherRemoveReq
	chPublisherAdd            chan pathPublisherAddReq
	chPublisherStart          chan pathPublisherStartReq
	chPublisherStop           chan pathPublisherStopReq
	chReaderAdd               chan pathReaderAddReq
	chReaderRemove            chan pathReaderRemoveReq
	chAPIPathsGet             chan pathAPIPathsGetReq

	// out
	done chan struct{}
}

func newPath(
	parentCtx context.Context,
	rtspAddress string,
	readTimeout conf.StringDuration,
	writeTimeout conf.StringDuration,
	readBufferCount int,
	udpMaxPayloadSize int,
	confName string,
	cnf *conf.PathConf,
	name string,
	matches []string,
	wg *sync.WaitGroup,
	externalCmdPool *externalcmd.Pool,
	parent pathParent,
) *path {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	pa := &path{
		rtspAddress:                    rtspAddress,
		readTimeout:                    readTimeout,
		writeTimeout:                   writeTimeout,
		readBufferCount:                readBufferCount,
		udpMaxPayloadSize:              udpMaxPayloadSize,
		confName:                       confName,
		conf:                           cnf,
		name:                           name,
		matches:                        matches,
		wg:                             wg,
		externalCmdPool:                externalCmdPool,
		parent:                         parent,
		ctx:                            ctx,
		ctxCancel:                      ctxCancel,
		bytesReceived:                  new(uint64),
		readers:                        make(map[reader]struct{}),
		onDemandStaticSourceReadyTimer: newEmptyTimer(),
		onDemandStaticSourceCloseTimer: newEmptyTimer(),
		onDemandPublisherReadyTimer:    newEmptyTimer(),
		onDemandPublisherCloseTimer:    newEmptyTimer(),
		chReloadConf:                   make(chan *conf.PathConf),
		chSourceStaticSetReady:         make(chan pathSourceStaticSetReadyReq),
		chSourceStaticSetNotReady:      make(chan pathSourceStaticSetNotReadyReq),
		chDescribe:                     make(chan pathDescribeReq),
		chPublisherRemove:              make(chan pathPublisherRemoveReq),
		chPublisherAdd:                 make(chan pathPublisherAddReq),
		chPublisherStart:               make(chan pathPublisherStartReq),
		chPublisherStop:                make(chan pathPublisherStopReq),
		chReaderAdd:                    make(chan pathReaderAddReq),
		chReaderRemove:                 make(chan pathReaderRemoveReq),
		chAPIPathsGet:                  make(chan pathAPIPathsGetReq),
		done:                           make(chan struct{}),
	}

	pa.Log(logger.Debug, "created")

	pa.wg.Add(1)
	go pa.run()

	return pa
}

func (pa *path) close() {
	pa.ctxCancel()
}

func (pa *path) wait() {
	<-pa.done
}

// Log is the main logging function.
func (pa *path) Log(level logger.Level, format string, args ...interface{}) {
	pa.parent.Log(level, "[path "+pa.name+"] "+format, args...)
}

func (pa *path) safeConf() *conf.PathConf {
	pa.confMutex.RLock()
	defer pa.confMutex.RUnlock()
	return pa.conf
}

func (pa *path) run() {
	defer close(pa.done)
	defer pa.wg.Done()

	if pa.conf.Source == "redirect" {
		pa.source = &sourceRedirect{}
	} else if pa.conf.HasStaticSource() {
		pa.source = newSourceStatic(
			pa.conf,
			pa.readTimeout,
			pa.writeTimeout,
			pa.readBufferCount,
			pa)

		if !pa.conf.SourceOnDemand {
			pa.source.(*sourceStatic).start()
		}
	}

	var onInitCmd *externalcmd.Cmd
	if pa.conf.RunOnInit != "" {
		pa.Log(logger.Info, "runOnInit command started")
		onInitCmd = externalcmd.NewCmd(
			pa.externalCmdPool,
			pa.conf.RunOnInit,
			pa.conf.RunOnInitRestart,
			pa.externalCmdEnv(),
			func(err error) {
				pa.Log(logger.Info, "runOnInit command exited: %v", err)
			})
	}

	err := func() error {
		for {
			select {
			case <-pa.onDemandStaticSourceReadyTimer.C:
				for _, req := range pa.describeRequestsOnHold {
					req.res <- pathDescribeRes{err: fmt.Errorf("source of path '%s' has timed out", pa.name)}
				}
				pa.describeRequestsOnHold = nil

				for _, req := range pa.readerAddRequestsOnHold {
					req.res <- pathReaderSetupPlayRes{err: fmt.Errorf("source of path '%s' has timed out", pa.name)}
				}
				pa.readerAddRequestsOnHold = nil

				pa.onDemandStaticSourceStop()

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case <-pa.onDemandStaticSourceCloseTimer.C:
				pa.sourceSetNotReady()
				pa.onDemandStaticSourceStop()

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case <-pa.onDemandPublisherReadyTimer.C:
				for _, req := range pa.describeRequestsOnHold {
					req.res <- pathDescribeRes{err: fmt.Errorf("source of path '%s' has timed out", pa.name)}
				}
				pa.describeRequestsOnHold = nil

				for _, req := range pa.readerAddRequestsOnHold {
					req.res <- pathReaderSetupPlayRes{err: fmt.Errorf("source of path '%s' has timed out", pa.name)}
				}
				pa.readerAddRequestsOnHold = nil

				pa.onDemandPublisherStop()

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case <-pa.onDemandPublisherCloseTimer.C:
				pa.onDemandPublisherStop()

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case newConf := <-pa.chReloadConf:
				if pa.conf.HasStaticSource() {
					go pa.source.(*sourceStatic).reloadConf(newConf)
				}

				pa.confMutex.Lock()
				pa.conf = newConf
				pa.confMutex.Unlock()

			case req := <-pa.chSourceStaticSetReady:
				err := pa.sourceSetReady(req.medias, req.generateRTPPackets)
				if err != nil {
					req.res <- pathSourceStaticSetReadyRes{err: err}
				} else {
					if pa.conf.HasOnDemandStaticSource() {
						pa.onDemandStaticSourceReadyTimer.Stop()
						pa.onDemandStaticSourceReadyTimer = newEmptyTimer()

						pa.onDemandStaticSourceScheduleClose()

						for _, req := range pa.describeRequestsOnHold {
							req.res <- pathDescribeRes{
								stream: pa.stream,
							}
						}
						pa.describeRequestsOnHold = nil

						for _, req := range pa.readerAddRequestsOnHold {
							pa.handleReaderAddPost(req)
						}
						pa.readerAddRequestsOnHold = nil
					}

					req.res <- pathSourceStaticSetReadyRes{stream: pa.stream}
				}

			case req := <-pa.chSourceStaticSetNotReady:
				pa.sourceSetNotReady()

				// send response before calling onDemandStaticSourceStop()
				// in order to avoid a deadlock due to sourceStatic.stop()
				close(req.res)

				if pa.conf.HasOnDemandStaticSource() && pa.onDemandStaticSourceState != pathOnDemandStateInitial {
					pa.onDemandStaticSourceStop()
				}

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case req := <-pa.chDescribe:
				pa.handleDescribe(req)

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case req := <-pa.chPublisherRemove:
				pa.handlePublisherRemove(req)

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case req := <-pa.chPublisherAdd:
				pa.handlePublisherAdd(req)

			case req := <-pa.chPublisherStart:
				pa.handlePublisherStart(req)

			case req := <-pa.chPublisherStop:
				pa.handlePublisherStop(req)

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case req := <-pa.chReaderAdd:
				pa.handleReaderAdd(req)

				if pa.shouldClose() {
					return fmt.Errorf("not in use")
				}

			case req := <-pa.chReaderRemove:
				pa.handleReaderRemove(req)

			case req := <-pa.chAPIPathsGet:
				pa.handleAPIPathsGet(req)

			case <-pa.ctx.Done():
				return fmt.Errorf("terminated")
			}
		}
	}()

	// call before destroying context
	pa.parent.onPathClose(pa)

	pa.ctxCancel()

	pa.onDemandStaticSourceReadyTimer.Stop()
	pa.onDemandStaticSourceCloseTimer.Stop()
	pa.onDemandPublisherReadyTimer.Stop()
	pa.onDemandPublisherCloseTimer.Stop()

	if onInitCmd != nil {
		onInitCmd.Close()
		pa.Log(logger.Info, "runOnInit command stopped")
	}

	for _, req := range pa.describeRequestsOnHold {
		req.res <- pathDescribeRes{err: fmt.Errorf("terminated")}
	}

	for _, req := range pa.readerAddRequestsOnHold {
		req.res <- pathReaderSetupPlayRes{err: fmt.Errorf("terminated")}
	}

	if pa.stream != nil {
		pa.sourceSetNotReady()
	}

	if pa.source != nil {
		if source, ok := pa.source.(*sourceStatic); ok {
			source.close()
		} else if source, ok := pa.source.(publisher); ok {
			source.close()
		}
	}

	if pa.onDemandCmd != nil {
		pa.onDemandCmd.Close()
		pa.Log(logger.Info, "runOnDemand command stopped")
	}

	pa.Log(logger.Debug, "destroyed (%v)", err)
}

func (pa *path) shouldClose() bool {
	return pa.conf.Regexp != nil &&
		pa.source == nil &&
		len(pa.readers) == 0 &&
		len(pa.describeRequestsOnHold) == 0 &&
		len(pa.readerAddRequestsOnHold) == 0
}

func (pa *path) externalCmdEnv() externalcmd.Environment {
	_, port, _ := net.SplitHostPort(pa.rtspAddress)
	env := externalcmd.Environment{
		"MTX_PATH":  pa.name,
		"RTSP_PATH": pa.name, // deprecated
		"RTSP_PORT": port,
	}

	if len(pa.matches) > 1 {
		for i, ma := range pa.matches[1:] {
			env["G"+strconv.FormatInt(int64(i+1), 10)] = ma
		}
	}

	return env
}

func (pa *path) onDemandStaticSourceStart() {
	pa.source.(*sourceStatic).start()

	pa.onDemandStaticSourceReadyTimer.Stop()
	pa.onDemandStaticSourceReadyTimer = time.NewTimer(time.Duration(pa.conf.SourceOnDemandStartTimeout))

	pa.onDemandStaticSourceState = pathOnDemandStateWaitingReady
}

func (pa *path) onDemandStaticSourceScheduleClose() {
	pa.onDemandStaticSourceCloseTimer.Stop()
	pa.onDemandStaticSourceCloseTimer = time.NewTimer(time.Duration(pa.conf.SourceOnDemandCloseAfter))

	pa.onDemandStaticSourceState = pathOnDemandStateClosing
}

func (pa *path) onDemandStaticSourceStop() {
	if pa.onDemandStaticSourceState == pathOnDemandStateClosing {
		pa.onDemandStaticSourceCloseTimer.Stop()
		pa.onDemandStaticSourceCloseTimer = newEmptyTimer()
	}

	pa.onDemandStaticSourceState = pathOnDemandStateInitial

	pa.source.(*sourceStatic).stop()
}

func (pa *path) onDemandPublisherStart() {
	pa.Log(logger.Info, "runOnDemand command started")
	pa.onDemandCmd = externalcmd.NewCmd(
		pa.externalCmdPool,
		pa.conf.RunOnDemand,
		pa.conf.RunOnDemandRestart,
		pa.externalCmdEnv(),
		func(err error) {
			pa.Log(logger.Info, "runOnDemand command exited: %v", err)
		})

	pa.onDemandPublisherReadyTimer.Stop()
	pa.onDemandPublisherReadyTimer = time.NewTimer(time.Duration(pa.conf.RunOnDemandStartTimeout))

	pa.onDemandPublisherState = pathOnDemandStateWaitingReady
}

func (pa *path) onDemandPublisherScheduleClose() {
	pa.onDemandPublisherCloseTimer.Stop()
	pa.onDemandPublisherCloseTimer = time.NewTimer(time.Duration(pa.conf.RunOnDemandCloseAfter))

	pa.onDemandPublisherState = pathOnDemandStateClosing
}

func (pa *path) onDemandPublisherStop() {
	if pa.source != nil {
		pa.source.(publisher).close()
		pa.doPublisherRemove()
	}

	if pa.onDemandPublisherState == pathOnDemandStateClosing {
		pa.onDemandPublisherCloseTimer.Stop()
		pa.onDemandPublisherCloseTimer = newEmptyTimer()
	}

	pa.onDemandPublisherState = pathOnDemandStateInitial

	if pa.onDemandCmd != nil {
		pa.onDemandCmd.Close()
		pa.onDemandCmd = nil
		pa.Log(logger.Info, "runOnDemand command stopped")
	}
}

func (pa *path) sourceSetReady(medias media.Medias, allocateEncoder bool) error {
	stream, err := newStream(
		pa.udpMaxPayloadSize,
		medias,
		allocateEncoder,
		pa.bytesReceived,
		pa.source,
	)
	if err != nil {
		return err
	}

	pa.stream = stream

	if pa.conf.RunOnReady != "" {
		pa.Log(logger.Info, "runOnReady command started")
		pa.onReadyCmd = externalcmd.NewCmd(
			pa.externalCmdPool,
			pa.conf.RunOnReady,
			pa.conf.RunOnReadyRestart,
			pa.externalCmdEnv(),
			func(err error) {
				pa.Log(logger.Info, "runOnReady command exited: %v", err)
			})
	}

	pa.parent.pathSourceReady(pa)

	return nil
}

func (pa *path) sourceSetNotReady() {
	pa.parent.pathSourceNotReady(pa)

	for r := range pa.readers {
		pa.doReaderRemove(r)
		r.close()
	}

	if pa.onReadyCmd != nil {
		pa.onReadyCmd.Close()
		pa.onReadyCmd = nil
		pa.Log(logger.Info, "runOnReady command stopped")
	}

	if pa.stream != nil {
		pa.stream.close()
		pa.stream = nil
	}
}

func (pa *path) doReaderRemove(r reader) {
	delete(pa.readers, r)
}

func (pa *path) doPublisherRemove() {
	if pa.stream != nil {
		pa.sourceSetNotReady()
	}

	pa.source = nil
}

func (pa *path) handleDescribe(req pathDescribeReq) {
	if _, ok := pa.source.(*sourceRedirect); ok {
		req.res <- pathDescribeRes{
			redirect: pa.conf.SourceRedirect,
		}
		return
	}

	if pa.stream != nil {
		req.res <- pathDescribeRes{
			stream: pa.stream,
		}
		return
	}

	if pa.conf.HasOnDemandStaticSource() {
		if pa.onDemandStaticSourceState == pathOnDemandStateInitial {
			pa.onDemandStaticSourceStart()
		}
		pa.describeRequestsOnHold = append(pa.describeRequestsOnHold, req)
		return
	}

	if pa.conf.HasOnDemandPublisher() {
		if pa.onDemandPublisherState == pathOnDemandStateInitial {
			pa.onDemandPublisherStart()
		}
		pa.describeRequestsOnHold = append(pa.describeRequestsOnHold, req)
		return
	}

	if pa.conf.Fallback != "" {
		fallbackURL := func() string {
			if strings.HasPrefix(pa.conf.Fallback, "/") {
				ur := url.URL{
					Scheme: req.url.Scheme,
					User:   req.url.User,
					Host:   req.url.Host,
					Path:   pa.conf.Fallback,
				}
				return ur.String()
			}
			return pa.conf.Fallback
		}()
		req.res <- pathDescribeRes{redirect: fallbackURL}
		return
	}

	req.res <- pathDescribeRes{err: pathErrNoOnePublishing{pathName: pa.name}}
}

func (pa *path) handlePublisherRemove(req pathPublisherRemoveReq) {
	if pa.source == req.author {
		pa.doPublisherRemove()
	}
	close(req.res)
}

func (pa *path) handlePublisherAdd(req pathPublisherAddReq) {
	if pa.conf.Source != "publisher" {
		req.res <- pathPublisherAnnounceRes{
			err: fmt.Errorf("can't publish to path '%s' since 'source' is not 'publisher'", pa.name),
		}
		return
	}

	if pa.source != nil {
		if pa.conf.DisablePublisherOverride {
			req.res <- pathPublisherAnnounceRes{err: fmt.Errorf("someone is already publishing to path '%s'", pa.name)}
			return
		}

		pa.Log(logger.Info, "closing existing publisher")
		pa.source.(publisher).close()
		pa.doPublisherRemove()
	}

	pa.source = req.author

	req.res <- pathPublisherAnnounceRes{path: pa}
}

func (pa *path) handlePublisherStart(req pathPublisherStartReq) {
	if pa.source != req.author {
		req.res <- pathPublisherRecordRes{err: fmt.Errorf("publisher is not assigned to this path anymore")}
		return
	}

	err := pa.sourceSetReady(req.medias, req.generateRTPPackets)
	if err != nil {
		req.res <- pathPublisherRecordRes{err: err}
		return
	}

	if pa.conf.HasOnDemandPublisher() {
		pa.onDemandPublisherReadyTimer.Stop()
		pa.onDemandPublisherReadyTimer = newEmptyTimer()

		pa.onDemandPublisherScheduleClose()

		for _, req := range pa.describeRequestsOnHold {
			req.res <- pathDescribeRes{
				stream: pa.stream,
			}
		}
		pa.describeRequestsOnHold = nil

		for _, req := range pa.readerAddRequestsOnHold {
			pa.handleReaderAddPost(req)
		}
		pa.readerAddRequestsOnHold = nil
	}

	req.res <- pathPublisherRecordRes{stream: pa.stream}
}

func (pa *path) handlePublisherStop(req pathPublisherStopReq) {
	if req.author == pa.source && pa.stream != nil {
		pa.sourceSetNotReady()
	}
	close(req.res)
}

func (pa *path) handleReaderRemove(req pathReaderRemoveReq) {
	if _, ok := pa.readers[req.author]; ok {
		pa.doReaderRemove(req.author)
	}
	close(req.res)

	if len(pa.readers) == 0 {
		if pa.conf.HasOnDemandStaticSource() {
			if pa.onDemandStaticSourceState == pathOnDemandStateReady {
				pa.onDemandStaticSourceScheduleClose()
			}
		} else if pa.conf.HasOnDemandPublisher() {
			if pa.onDemandPublisherState == pathOnDemandStateReady {
				pa.onDemandPublisherScheduleClose()
			}
		}
	}
}

func (pa *path) handleReaderAdd(req pathReaderAddReq) {
	if pa.stream != nil {
		pa.handleReaderAddPost(req)
		return
	}

	if pa.conf.HasOnDemandStaticSource() {
		if pa.onDemandStaticSourceState == pathOnDemandStateInitial {
			pa.onDemandStaticSourceStart()
		}
		pa.readerAddRequestsOnHold = append(pa.readerAddRequestsOnHold, req)
		return
	}

	if pa.conf.HasOnDemandPublisher() {
		if pa.onDemandPublisherState == pathOnDemandStateInitial {
			pa.onDemandPublisherStart()
		}
		pa.readerAddRequestsOnHold = append(pa.readerAddRequestsOnHold, req)
		return
	}

	req.res <- pathReaderSetupPlayRes{err: pathErrNoOnePublishing{pathName: pa.name}}
}

func (pa *path) handleReaderAddPost(req pathReaderAddReq) {
	pa.readers[req.author] = struct{}{}

	if pa.conf.HasOnDemandStaticSource() {
		if pa.onDemandStaticSourceState == pathOnDemandStateClosing {
			pa.onDemandStaticSourceState = pathOnDemandStateReady
			pa.onDemandStaticSourceCloseTimer.Stop()
			pa.onDemandStaticSourceCloseTimer = newEmptyTimer()
		}
	} else if pa.conf.HasOnDemandPublisher() {
		if pa.onDemandPublisherState == pathOnDemandStateClosing {
			pa.onDemandPublisherState = pathOnDemandStateReady
			pa.onDemandPublisherCloseTimer.Stop()
			pa.onDemandPublisherCloseTimer = newEmptyTimer()
		}
	}

	req.res <- pathReaderSetupPlayRes{
		path:   pa,
		stream: pa.stream,
	}
}

func (pa *path) handleAPIPathsGet(req pathAPIPathsGetReq) {
	req.res <- pathAPIPathsGetRes{
		data: &apiPath{
			Name:     pa.name,
			ConfName: pa.confName,
			Conf:     pa.conf,
			Source: func() interface{} {
				if pa.source == nil {
					return nil
				}
				return pa.source.apiSourceDescribe()
			}(),
			SourceReady: pa.stream != nil,
			Tracks: func() []string {
				if pa.stream == nil {
					return []string{}
				}
				return mediasDescription(pa.stream.medias())
			}(),
			BytesReceived: atomic.LoadUint64(pa.bytesReceived),
			Readers: func() []interface{} {
				ret := []interface{}{}
				for r := range pa.readers {
					ret = append(ret, r.apiReaderDescribe())
				}
				return ret
			}(),
		},
	}
}

// reloadConf is called by pathManager.
func (pa *path) reloadConf(newConf *conf.PathConf) {
	select {
	case pa.chReloadConf <- newConf:
	case <-pa.ctx.Done():
	}
}

// sourceStaticSetReady is called by sourceStatic.
func (pa *path) sourceStaticSetReady(sourceStaticCtx context.Context, req pathSourceStaticSetReadyReq) {
	select {
	case pa.chSourceStaticSetReady <- req:

	case <-pa.ctx.Done():
		req.res <- pathSourceStaticSetReadyRes{err: fmt.Errorf("terminated")}

	// this avoids:
	// - invalid requests sent after the source has been terminated
	// - deadlocks caused by <-done inside stop()
	case <-sourceStaticCtx.Done():
		req.res <- pathSourceStaticSetReadyRes{err: fmt.Errorf("terminated")}
	}
}

// sourceStaticSetNotReady is called by sourceStatic.
func (pa *path) sourceStaticSetNotReady(sourceStaticCtx context.Context, req pathSourceStaticSetNotReadyReq) {
	select {
	case pa.chSourceStaticSetNotReady <- req:

	case <-pa.ctx.Done():
		close(req.res)

	// this avoids:
	// - invalid requests sent after the source has been terminated
	// - deadlocks caused by <-done inside stop()
	case <-sourceStaticCtx.Done():
		close(req.res)
	}
}

// describe is called by a reader or publisher through pathManager.
func (pa *path) describe(req pathDescribeReq) pathDescribeRes {
	select {
	case pa.chDescribe <- req:
		return <-req.res
	case <-pa.ctx.Done():
		return pathDescribeRes{err: fmt.Errorf("terminated")}
	}
}

// publisherRemove is called by a publisher.
func (pa *path) publisherRemove(req pathPublisherRemoveReq) {
	req.res = make(chan struct{})
	select {
	case pa.chPublisherRemove <- req:
		<-req.res
	case <-pa.ctx.Done():
	}
}

// publisherAdd is called by a publisher through pathManager.
func (pa *path) publisherAdd(req pathPublisherAddReq) pathPublisherAnnounceRes {
	select {
	case pa.chPublisherAdd <- req:
		return <-req.res
	case <-pa.ctx.Done():
		return pathPublisherAnnounceRes{err: fmt.Errorf("terminated")}
	}
}

// publisherStart is called by a publisher.
func (pa *path) publisherStart(req pathPublisherStartReq) pathPublisherRecordRes {
	req.res = make(chan pathPublisherRecordRes)
	select {
	case pa.chPublisherStart <- req:
		return <-req.res
	case <-pa.ctx.Done():
		return pathPublisherRecordRes{err: fmt.Errorf("terminated")}
	}
}

// publisherStop is called by a publisher.
func (pa *path) publisherStop(req pathPublisherStopReq) {
	req.res = make(chan struct{})
	select {
	case pa.chPublisherStop <- req:
		<-req.res
	case <-pa.ctx.Done():
	}
}

// readerAdd is called by a reader through pathManager.
func (pa *path) readerAdd(req pathReaderAddReq) pathReaderSetupPlayRes {
	select {
	case pa.chReaderAdd <- req:
		return <-req.res
	case <-pa.ctx.Done():
		return pathReaderSetupPlayRes{err: fmt.Errorf("terminated")}
	}
}

// readerRemove is called by a reader.
func (pa *path) readerRemove(req pathReaderRemoveReq) {
	req.res = make(chan struct{})
	select {
	case pa.chReaderRemove <- req:
		<-req.res
	case <-pa.ctx.Done():
	}
}

// apiPathsGet is called by api.
func (pa *path) apiPathsGet(req pathAPIPathsGetReq) (*apiPath, error) {
	req.res = make(chan pathAPIPathsGetRes)
	select {
	case pa.chAPIPathsGet <- req:
		res := <-req.res
		return res.data, res.err

	case <-pa.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}
