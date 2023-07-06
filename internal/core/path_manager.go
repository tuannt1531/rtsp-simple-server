package core

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
)

func pathConfCanBeUpdated(oldPathConf *conf.PathConf, newPathConf *conf.PathConf) bool {
	clone := oldPathConf.Clone()

	clone.RPICameraBrightness = newPathConf.RPICameraBrightness
	clone.RPICameraContrast = newPathConf.RPICameraContrast
	clone.RPICameraSaturation = newPathConf.RPICameraSaturation
	clone.RPICameraSharpness = newPathConf.RPICameraSharpness
	clone.RPICameraExposure = newPathConf.RPICameraExposure
	clone.RPICameraAWB = newPathConf.RPICameraAWB
	clone.RPICameraDenoise = newPathConf.RPICameraDenoise
	clone.RPICameraShutter = newPathConf.RPICameraShutter
	clone.RPICameraMetering = newPathConf.RPICameraMetering
	clone.RPICameraGain = newPathConf.RPICameraGain
	clone.RPICameraEV = newPathConf.RPICameraEV
	clone.RPICameraFPS = newPathConf.RPICameraFPS

	return newPathConf.Equal(clone)
}

type pathManagerHLSManager interface {
	pathSourceReady(*path)
	pathSourceNotReady(*path)
}

type pathManagerParent interface {
	logger.Writer
}

type pathManager struct {
	externalAuthenticationURL string
	rtspAddress               string
	authMethods               conf.AuthMethods
	readTimeout               conf.StringDuration
	writeTimeout              conf.StringDuration
	readBufferCount           int
	udpMaxPayloadSize         int
	pathConfs                 map[string]*conf.PathConf
	externalCmdPool           *externalcmd.Pool
	metrics                   *metrics
	parent                    pathManagerParent

	ctx         context.Context
	ctxCancel   func()
	wg          sync.WaitGroup
	hlsManager  pathManagerHLSManager
	paths       map[string]*path
	pathsByConf map[string]map[*path]struct{}

	// in
	chConfReload         chan map[string]*conf.PathConf
	chPathClose          chan *path
	chPathSourceReady    chan *path
	chPathSourceNotReady chan *path
	chPathGetPathConf    chan pathGetPathConfReq
	chDescribe           chan pathDescribeReq
	chReaderAdd          chan pathReaderAddReq
	chPublisherAdd       chan pathPublisherAddReq
	chHLSManagerSet      chan pathManagerHLSManager
	chAPIPathsList       chan pathAPIPathsListReq
	chAPIPathsGet        chan pathAPIPathsGetReq
}

func newPathManager(
	parentCtx context.Context,
	externalAuthenticationURL string,
	rtspAddress string,
	authMethods conf.AuthMethods,
	readTimeout conf.StringDuration,
	writeTimeout conf.StringDuration,
	readBufferCount int,
	udpMaxPayloadSize int,
	pathConfs map[string]*conf.PathConf,
	externalCmdPool *externalcmd.Pool,
	metrics *metrics,
	parent pathManagerParent,
) *pathManager {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	pm := &pathManager{
		externalAuthenticationURL: externalAuthenticationURL,
		rtspAddress:               rtspAddress,
		authMethods:               authMethods,
		readTimeout:               readTimeout,
		writeTimeout:              writeTimeout,
		readBufferCount:           readBufferCount,
		udpMaxPayloadSize:         udpMaxPayloadSize,
		pathConfs:                 pathConfs,
		externalCmdPool:           externalCmdPool,
		metrics:                   metrics,
		parent:                    parent,
		ctx:                       ctx,
		ctxCancel:                 ctxCancel,
		paths:                     make(map[string]*path),
		pathsByConf:               make(map[string]map[*path]struct{}),
		chConfReload:              make(chan map[string]*conf.PathConf),
		chPathClose:               make(chan *path),
		chPathSourceReady:         make(chan *path),
		chPathSourceNotReady:      make(chan *path),
		chPathGetPathConf:         make(chan pathGetPathConfReq),
		chDescribe:                make(chan pathDescribeReq),
		chReaderAdd:               make(chan pathReaderAddReq),
		chPublisherAdd:            make(chan pathPublisherAddReq),
		chHLSManagerSet:           make(chan pathManagerHLSManager),
		chAPIPathsList:            make(chan pathAPIPathsListReq),
		chAPIPathsGet:             make(chan pathAPIPathsGetReq),
	}

	for pathConfName, pathConf := range pm.pathConfs {
		if pathConf.Regexp == nil {
			pm.createPath(pathConfName, pathConf, pathConfName, nil)
		}
	}

	if pm.metrics != nil {
		pm.metrics.pathManagerSet(pm)
	}

	pm.Log(logger.Debug, "path manager created")

	pm.wg.Add(1)
	go pm.run()

	return pm
}

func (pm *pathManager) close() {
	pm.Log(logger.Debug, "path manager is shutting down")
	pm.ctxCancel()
	pm.wg.Wait()
}

// Log is the main logging function.
func (pm *pathManager) Log(level logger.Level, format string, args ...interface{}) {
	pm.parent.Log(level, format, args...)
}

func (pm *pathManager) run() {
	defer pm.wg.Done()

outer:
	for {
		select {
		case newPathConfs := <-pm.chConfReload:
			for confName, pathConf := range pm.pathConfs {
				if newPathConf, ok := newPathConfs[confName]; ok {
					// configuration has changed
					if !newPathConf.Equal(pathConf) {
						if pathConfCanBeUpdated(pathConf, newPathConf) { // paths associated with the configuration can be updated
							for pa := range pm.pathsByConf[confName] {
								go pa.reloadConf(newPathConf)
							}
						} else { // paths associated with the configuration must be recreated
							for pa := range pm.pathsByConf[confName] {
								pm.removePath(pa)
								pa.close()
								pa.wait() // avoid conflicts between sources
							}
						}
					}
				} else {
					// configuration has been deleted, remove associated paths
					for pa := range pm.pathsByConf[confName] {
						pm.removePath(pa)
						pa.close()
						pa.wait() // avoid conflicts between sources
					}
				}
			}

			pm.pathConfs = newPathConfs

			// add new paths
			for pathConfName, pathConf := range pm.pathConfs {
				if _, ok := pm.paths[pathConfName]; !ok && pathConf.Regexp == nil {
					pm.createPath(pathConfName, pathConf, pathConfName, nil)
				}
			}

		case pa := <-pm.chPathClose:
			if pmpa, ok := pm.paths[pa.name]; !ok || pmpa != pa {
				continue
			}
			pm.removePath(pa)

		case pa := <-pm.chPathSourceReady:
			if pm.hlsManager != nil {
				pm.hlsManager.pathSourceReady(pa)
			}

		case pa := <-pm.chPathSourceNotReady:
			if pm.hlsManager != nil {
				pm.hlsManager.pathSourceNotReady(pa)
			}

		case req := <-pm.chPathGetPathConf:
			_, pathConf, _, err := pm.getPathConfInternal(req.name)
			if err != nil {
				req.res <- pathGetPathConfRes{err: err}
				continue
			}

			err = authenticate(pm.externalAuthenticationURL, pm.authMethods,
				req.name, pathConf, req.publish, req.credentials)
			if err != nil {
				req.res <- pathGetPathConfRes{err: pathErrAuth{wrapped: err}}
				continue
			}

			req.res <- pathGetPathConfRes{conf: pathConf}

		case req := <-pm.chDescribe:
			pathConfName, pathConf, pathMatches, err := pm.getPathConfInternal(req.pathName)
			if err != nil {
				req.res <- pathDescribeRes{err: err}
				continue
			}

			err = authenticate(pm.externalAuthenticationURL, pm.authMethods, req.pathName, pathConf, false, req.credentials)
			if err != nil {
				req.res <- pathDescribeRes{err: pathErrAuth{wrapped: err}}
				continue
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[req.pathName]; !ok {
				pm.createPath(pathConfName, pathConf, req.pathName, pathMatches)
			}

			req.res <- pathDescribeRes{path: pm.paths[req.pathName]}

		case req := <-pm.chReaderAdd:
			pathConfName, pathConf, pathMatches, err := pm.getPathConfInternal(req.pathName)
			if err != nil {
				req.res <- pathReaderSetupPlayRes{err: err}
				continue
			}

			if !req.skipAuth {
				err = authenticate(pm.externalAuthenticationURL, pm.authMethods, req.pathName, pathConf, false, req.credentials)
				if err != nil {
					req.res <- pathReaderSetupPlayRes{err: pathErrAuth{wrapped: err}}
					continue
				}
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[req.pathName]; !ok {
				pm.createPath(pathConfName, pathConf, req.pathName, pathMatches)
			}

			req.res <- pathReaderSetupPlayRes{path: pm.paths[req.pathName]}

		case req := <-pm.chPublisherAdd:
			pathConfName, pathConf, pathMatches, err := pm.getPathConfInternal(req.pathName)
			if err != nil {
				req.res <- pathPublisherAnnounceRes{err: err}
				continue
			}

			if !req.skipAuth {
				err = authenticate(pm.externalAuthenticationURL, pm.authMethods, req.pathName, pathConf, true, req.credentials)
				if err != nil {
					req.res <- pathPublisherAnnounceRes{err: pathErrAuth{wrapped: err}}
					continue
				}
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[req.pathName]; !ok {
				pm.createPath(pathConfName, pathConf, req.pathName, pathMatches)
			}

			req.res <- pathPublisherAnnounceRes{path: pm.paths[req.pathName]}

		case s := <-pm.chHLSManagerSet:
			pm.hlsManager = s

		case req := <-pm.chAPIPathsList:
			paths := make(map[string]*path)

			for name, pa := range pm.paths {
				paths[name] = pa
			}

			req.res <- pathAPIPathsListRes{paths: paths}

		case req := <-pm.chAPIPathsGet:
			path, ok := pm.paths[req.name]
			if !ok {
				req.res <- pathAPIPathsGetRes{err: errAPINotFound}
				continue
			}

			req.res <- pathAPIPathsGetRes{path: path}

		case <-pm.ctx.Done():
			break outer
		}
	}

	pm.ctxCancel()

	if pm.metrics != nil {
		pm.metrics.pathManagerSet(nil)
	}
}

func (pm *pathManager) createPath(
	pathConfName string,
	pathConf *conf.PathConf,
	name string,
	matches []string,
) {
	pa := newPath(
		pm.ctx,
		pm.rtspAddress,
		pm.readTimeout,
		pm.writeTimeout,
		pm.readBufferCount,
		pm.udpMaxPayloadSize,
		pathConfName,
		pathConf,
		name,
		matches,
		&pm.wg,
		pm.externalCmdPool,
		pm)

	pm.paths[name] = pa

	if _, ok := pm.pathsByConf[pathConfName]; !ok {
		pm.pathsByConf[pathConfName] = make(map[*path]struct{})
	}
	pm.pathsByConf[pathConfName][pa] = struct{}{}
}

func (pm *pathManager) removePath(pa *path) {
	delete(pm.pathsByConf[pa.confName], pa)
	if len(pm.pathsByConf[pa.confName]) == 0 {
		delete(pm.pathsByConf, pa.confName)
	}
	delete(pm.paths, pa.name)
}

func (pm *pathManager) getPathConfInternal(name string) (string, *conf.PathConf, []string, error) {
	err := conf.IsValidPathName(name)
	if err != nil {
		return "", nil, nil, fmt.Errorf("invalid path name: %s (%s)", err, name)
	}

	// normal path
	if pathConf, ok := pm.pathConfs[name]; ok {
		return name, pathConf, nil, nil
	}

	// regular expression path
	for pathConfName, pathConf := range pm.pathConfs {
		if pathConf.Regexp != nil {
			m := pathConf.Regexp.FindStringSubmatch(name)
			if m != nil {
				return pathConfName, pathConf, m, nil
			}
		}
	}

	return "", nil, nil, fmt.Errorf("path '%s' is not configured", name)
}

// confReload is called by core.
func (pm *pathManager) confReload(pathConfs map[string]*conf.PathConf) {
	select {
	case pm.chConfReload <- pathConfs:
	case <-pm.ctx.Done():
	}
}

// pathSourceReady is called by path.
func (pm *pathManager) pathSourceReady(pa *path) {
	select {
	case pm.chPathSourceReady <- pa:
	case <-pm.ctx.Done():
	case <-pa.ctx.Done(): // in case pathManager is blocked by path.wait()
	}
}

// pathSourceNotReady is called by path.
func (pm *pathManager) pathSourceNotReady(pa *path) {
	select {
	case pm.chPathSourceNotReady <- pa:
	case <-pm.ctx.Done():
	case <-pa.ctx.Done(): // in case pathManager is blocked by path.wait()
	}
}

// onPathClose is called by path.
func (pm *pathManager) onPathClose(pa *path) {
	select {
	case pm.chPathClose <- pa:
	case <-pm.ctx.Done():
	case <-pa.ctx.Done(): // in case pathManager is blocked by path.wait()
	}
}

// getPathConf is called by a reader or publisher.
func (pm *pathManager) getPathConf(req pathGetPathConfReq) pathGetPathConfRes {
	req.res = make(chan pathGetPathConfRes)
	select {
	case pm.chPathGetPathConf <- req:
		return <-req.res

	case <-pm.ctx.Done():
		return pathGetPathConfRes{err: fmt.Errorf("terminated")}
	}
}

// describe is called by a reader or publisher.
func (pm *pathManager) describe(req pathDescribeReq) pathDescribeRes {
	req.res = make(chan pathDescribeRes)
	select {
	case pm.chDescribe <- req:
		res1 := <-req.res
		if res1.err != nil {
			return res1
		}

		res2 := res1.path.describe(req)
		if res2.err != nil {
			return res2
		}

		res2.path = res1.path
		return res2

	case <-pm.ctx.Done():
		return pathDescribeRes{err: fmt.Errorf("terminated")}
	}
}

// publisherAnnounce is called by a publisher.
func (pm *pathManager) publisherAdd(req pathPublisherAddReq) pathPublisherAnnounceRes {
	req.res = make(chan pathPublisherAnnounceRes)
	select {
	case pm.chPublisherAdd <- req:
		res := <-req.res
		if res.err != nil {
			return res
		}

		return res.path.publisherAdd(req)

	case <-pm.ctx.Done():
		return pathPublisherAnnounceRes{err: fmt.Errorf("terminated")}
	}
}

// readerSetupPlay is called by a reader.
func (pm *pathManager) readerAdd(req pathReaderAddReq) pathReaderSetupPlayRes {
	req.res = make(chan pathReaderSetupPlayRes)
	select {
	case pm.chReaderAdd <- req:
		res := <-req.res
		if res.err != nil {
			return res
		}

		return res.path.readerAdd(req)

	case <-pm.ctx.Done():
		return pathReaderSetupPlayRes{err: fmt.Errorf("terminated")}
	}
}

// hlsManagerSet is called by hlsManager.
func (pm *pathManager) hlsManagerSet(s pathManagerHLSManager) {
	select {
	case pm.chHLSManagerSet <- s:
	case <-pm.ctx.Done():
	}
}

// apiPathsList is called by api.
func (pm *pathManager) apiPathsList() (*apiPathsList, error) {
	req := pathAPIPathsListReq{
		res: make(chan pathAPIPathsListRes),
	}

	select {
	case pm.chAPIPathsList <- req:
		res := <-req.res

		res.data = &apiPathsList{
			Items: []*apiPath{},
		}

		for _, pa := range res.paths {
			item, err := pa.apiPathsGet(pathAPIPathsGetReq{})
			if err == nil {
				res.data.Items = append(res.data.Items, item)
			}
		}

		sort.Slice(res.data.Items, func(i, j int) bool {
			return res.data.Items[i].Name < res.data.Items[j].Name
		})

		return res.data, nil

	case <-pm.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// apiPathsGet is called by api.
func (pm *pathManager) apiPathsGet(name string) (*apiPath, error) {
	req := pathAPIPathsGetReq{
		name: name,
		res:  make(chan pathAPIPathsGetRes),
	}

	select {
	case pm.chAPIPathsGet <- req:
		res := <-req.res
		if res.err != nil {
			return nil, res.err
		}

		data, err := res.path.apiPathsGet(req)
		return data, err

	case <-pm.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}
