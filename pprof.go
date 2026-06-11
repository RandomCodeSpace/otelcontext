package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" // registers profiling handlers on http.DefaultServeMux
	"time"
)

// startPprofServer serves net/http/pprof on its own listener so profiling
// never leaks onto the public :8080 mux (which is a fresh NewServeMux).
// Empty addr disables it. Bind to loopback in production; heap profiles
// expose internals.
func startPprofServer(addr string, log *slog.Logger) (*http.Server, net.Addr, error) {
	if addr == "" {
		return nil, nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler:           http.DefaultServeMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("pprof server exited", "error", err)
		}
	}()
	log.Info("pprof profiling enabled", "addr", ln.Addr().String())
	return srv, ln.Addr(), nil
}
