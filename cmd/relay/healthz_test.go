package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakePinger is a test double for the pinger interface.
type fakePinger struct {
	err   error
	delay time.Duration
}

func (f *fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

type healthzResponse struct {
	Status   string            `json:"status"`
	Backends map[string]string `json:"backends"`
}

func doHealthz(t *testing.T, backends map[string]pinger, deadlineMS int) *healthzResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	healthzHandler(backends, deadlineMS, false).ServeHTTP(w, req)
	var resp healthzResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return &resp
}

func TestHealthz_AllOK(t *testing.T) {
	backends := map[string]pinger{
		"catalog":  nil, // memory/yaml — always ok
		"state":    nil, // memory — always ok
		"eventlog": nil, // file — always ok
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	healthzHandler(backends, 500, false).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp healthzResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "ok" {
		t.Errorf("expected status ok, got %q", resp.Status)
	}
	for name, status := range resp.Backends {
		if status != "ok" {
			t.Errorf("backend %q: expected ok, got %q", name, status)
		}
	}
}

func TestHealthz_OnePingErrors_Returns503(t *testing.T) {
	backends := map[string]pinger{
		"catalog":  nil,
		"state":    &fakePinger{err: errors.New("connection refused")},
		"eventlog": nil,
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	healthzHandler(backends, 500, false).ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	resp := doHealthz(t, backends, 500)
	if resp.Status != "degraded" {
		t.Errorf("expected degraded, got %q", resp.Status)
	}
	if resp.Backends["catalog"] != "ok" {
		t.Errorf("catalog should be ok")
	}
	if resp.Backends["state"] == "ok" {
		t.Errorf("state should be error")
	}
}

func TestHealthz_SlowPing_DeadlineEnforced(t *testing.T) {
	backends := map[string]pinger{
		"catalog":  nil,
		"state":    &fakePinger{delay: 2 * time.Second}, // hangs
		"eventlog": nil,
	}
	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	healthzHandler(backends, 300, false).ServeHTTP(w, req) // 300ms deadline
	elapsed := time.Since(start)

	if elapsed > 600*time.Millisecond {
		t.Errorf("healthz took %v, expected < 600ms", elapsed)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestHealthz_NilPingers_AlwaysOK(t *testing.T) {
	backends := map[string]pinger{
		"catalog":  nil,
		"state":    nil,
		"eventlog": nil,
	}
	resp := doHealthz(t, backends, 500)
	if resp.Status != "ok" {
		t.Errorf("all nil pingers should yield ok, got %q", resp.Status)
	}
}
