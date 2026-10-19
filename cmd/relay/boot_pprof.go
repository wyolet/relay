package main

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/wyolet/relay/internal/config"
)

// pprofMux serves only the runtime profiling handlers. Importing net/http/pprof also registers them on http.DefaultServeMux, so no listener may serve that mux.
func pprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// startPprof serves the profiling handlers on their own listener; it returns a nil server and channel when RELAY_PPROF_ADDR is unset.
func startPprof(cfg *config.Config) (*http.Server, <-chan error) {
	if cfg.PprofAddr == "" {
		return nil, nil
	}
	srv := &http.Server{
		Addr:              cfg.PprofAddr,
		Handler:           pprofMux(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout stays 0: /profile and /trace write for as many seconds as the caller asks.
	}
	slog.Info("relay pprof listening", "addr", srv.Addr)
	_, errs := serve(srv)
	return srv, errs
}
