package core

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
)

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

// exit when there's a panic inside HTTP handlers.
// https://github.com/golang/go/issues/16542
type exitOnPanicHandler struct {
	http.Handler
}

func (h exitOnPanicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		err := recover()
		if err != nil {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr, "panic: %v\n\n%s", err, buf[:n])
			os.Exit(1)
		}
	}()
	h.Handler.ServeHTTP(w, r)
}

type httpServer struct {
	ln    net.Listener
	inner *http.Server
}

func newHTTPServer(
	address string,
	readTimeout conf.StringDuration,
	serverCert string,
	serverKey string,
	handler http.Handler,
) (*httpServer, error) {
	ln, err := net.Listen(restrictNetwork("tcp", address))
	if err != nil {
		return nil, err
	}

	var tlsConfig *tls.Config
	if serverCert != "" {
		crt, err := tls.LoadX509KeyPair(serverCert, serverKey)
		if err != nil {
			ln.Close()
			return nil, err
		}

		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{crt},
		}
	}

	s := &httpServer{
		ln: ln,
		inner: &http.Server{
			Handler:           exitOnPanicHandler{handler},
			TLSConfig:         tlsConfig,
			ReadHeaderTimeout: time.Duration(readTimeout),
			ErrorLog:          log.New(&nilWriter{}, "", 0),
		},
	}

	if tlsConfig != nil {
		go s.inner.ServeTLS(s.ln, "", "")
	} else {
		go s.inner.Serve(s.ln)
	}

	return s, nil
}

func (s *httpServer) close() {
	s.inner.Shutdown(context.Background())
	s.ln.Close() // in case Shutdown() is called before Serve()
}
