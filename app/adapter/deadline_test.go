package adapter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
)

// buildTimeoutSpec returns a spec whose deadlines are shortened to test speed. The fields are unexported and set post-Build on purpose: the production values come from the constants + SetUpstreamStreamIdleTimeout.
func buildTimeoutSpec(t *testing.T, sync, idle time.Duration) *Spec {
	t.Helper()
	s := (&Spec{Name: adapters.OpenAI, DefaultPath: "/v1/chat/completions"}).Build()
	s.syncTimeout = sync
	s.idleTimeout = idle
	return s
}

func callSpec(t *testing.T, s *Spec, baseURL string, stream bool) (*http.Response, error) {
	t.Helper()
	return s.PipelineAdapter().Call(t.Context(), baseURL, nil, "", []byte(`{}`), nil, "test-model", stream, false)
}

// A stream that keeps trickling bytes must outlive the buffered-call deadline: the 5-minute cap is a header/total-body bound, never a cap on generation length.
func TestCall_StreamOutlivesSyncTimeout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for range 8 {
			_, _ = io.WriteString(w, "data: tick\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
	}))
	defer up.Close()

	s := buildTimeoutSpec(t, 50*time.Millisecond, 2*time.Second)
	resp, err := callSpec(t, s, up.URL, true)
	if err != nil {
		t.Fatalf("stream call: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream body cut short after %d bytes: %v", len(body), err)
	}
	if len(body) == 0 {
		t.Fatal("stream body empty")
	}
}

// Silence past the idle deadline ends the stream — the caller sees a read error, not a hang.
func TestCall_StreamIdleTimeoutEndsBody(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer up.Close()
	defer close(release)

	s := buildTimeoutSpec(t, 5*time.Second, 100*time.Millisecond)
	resp, err := callSpec(t, s, up.URL, true)
	if err != nil {
		t.Fatalf("stream call: %v", err)
	}
	defer resp.Body.Close()

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("silent upstream: read returned nil error, want the idle deadline")
	}
}

// A buffered call keeps a total deadline, and it must still be armed while the handler reads the body — which happens after Call has returned.
func TestCall_SyncTimeoutCoversBodyRead(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer up.Close()
	defer close(release)

	s := buildTimeoutSpec(t, 100*time.Millisecond, 0)
	resp, err := callSpec(t, s, up.URL, false)
	if err != nil {
		t.Fatalf("sync call: %v", err)
	}
	defer resp.Body.Close()

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("stalled upstream: read returned nil error, want the total deadline")
	}
}

// Closing the body releases the derived context, so nothing leaks a timer or a cancel for the life of the process.
func TestCall_BodyCloseCancelsContext(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	for _, stream := range []bool{false, true} {
		s := buildTimeoutSpec(t, time.Minute, time.Minute)
		resp, err := callSpec(t, s, up.URL, stream)
		if err != nil {
			t.Fatalf("call(stream=%v): %v", stream, err)
		}
		_, _ = io.ReadAll(resp.Body)
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close(stream=%v): %v", stream, err)
		}
		if err := resp.Request.Context().Err(); err != context.Canceled {
			t.Fatalf("context after close(stream=%v) = %v, want canceled", stream, err)
		}
	}
}

// stalledBodyUpstream sends its headers, then nothing until the test ends.
func stalledBodyUpstream(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })
	return up.URL
}

// A caller that already set the sync cap or a tighter one (the pipeline, via CallTimeout) gets no second context: its own deadline bounds the body read.
func TestCall_CallerDeadlineIsReused(t *testing.T) {
	s := buildTimeoutSpec(t, time.Minute, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	resp, err := s.PipelineAdapter().Call(ctx, stalledBodyUpstream(t), nil, "", []byte(`{}`), nil, "test-model", false, false)
	if err != nil {
		t.Fatalf("sync call: %v", err)
	}
	defer resp.Body.Close()
	if _, derived := resp.Body.(*cancelOnCloseBody); derived {
		t.Fatal("adapter derived a context under a caller deadline inside its cap")
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("stalled upstream: read returned nil error, want the caller's deadline")
	}
}

// A caller deadline past the sync cap does not loosen it.
func TestCall_LaterCallerDeadlineKeepsCap(t *testing.T) {
	s := buildTimeoutSpec(t, 100*time.Millisecond, 0)
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	resp, err := s.PipelineAdapter().Call(ctx, stalledBodyUpstream(t), nil, "", []byte(`{}`), nil, "test-model", false, false)
	if err != nil {
		t.Fatalf("sync call: %v", err)
	}
	defer resp.Body.Close()
	start := time.Now()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("stalled upstream: read returned nil error, want the sync cap")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("read ended after %v, want about the 100ms cap", waited)
	}
}

func TestCallTimeout(t *testing.T) {
	a := buildTimeoutSpec(t, 3*time.Second, time.Second).PipelineAdapter().(*specAdapter)
	if got := a.CallTimeout(false); got != 3*time.Second {
		t.Errorf("CallTimeout(buffered) = %v, want the sync cap", got)
	}
	if got := a.CallTimeout(true); got != 0 {
		t.Errorf("CallTimeout(stream) = %v, want none (the idle timer bounds a stream)", got)
	}
}
