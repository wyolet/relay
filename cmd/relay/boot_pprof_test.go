package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/internal/config"
)

func TestPprofMuxServesProfiles(t *testing.T) {
	mux := pprofMux()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/heap?debug=1", "/debug/pprof/goroutine?debug=1"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// Importing net/http/pprof registers its handlers on http.DefaultServeMux; the inference and control routers must not fall through to it.
func TestPprofNotOnInferenceOrControl(t *testing.T) {
	probe := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if probe.Code != http.StatusOK {
		t.Fatalf("DefaultServeMux GET /debug/pprof/ = %d; the check below would pass vacuously", probe.Code)
	}

	inferRouter := chi.NewRouter()
	inference.Mount(inferRouter, inference.Deps{})
	ctrlRouter := chi.NewRouter()
	control.Mount(ctrlRouter, control.Deps{Stores: &appcatalog.Stores{}})
	for name, h := range map[string]http.Handler{"inference": inferRouter, "control": ctrlRouter} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: GET /debug/pprof/ = %d, want 404", name, rec.Code)
		}
	}
}

func TestStartPprof(t *testing.T) {
	if srv, errs := startPprof(&config.Config{}); srv != nil || errs != nil {
		t.Fatal("startPprof started a listener with RELAY_PPROF_ADDR unset")
	}

	// startPprof does not report the bound address, so take a free port first.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv, errs := startPprof(&config.Config{PprofAddr: addr})
	resp, err := http.Get("http://" + addr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "goroutine") {
		t.Fatalf("GET /debug/pprof/ = %d", resp.StatusCode)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve returned %v, want ErrServerClosed", err)
	}
}
