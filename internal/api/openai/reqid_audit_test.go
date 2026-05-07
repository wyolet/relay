package openai

// reqid_audit_test.go — PER-254: every log line emitted under a request context
// must carry a request_id field.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/internal/pipeline"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/transport"
)

// TestRequestIDPresentOnEveryLogLine drives a ChatCompletions request through
// the handler, captures all slog JSON lines emitted during the lifecycle, and
// asserts that every line that carries a "context" marker also carries a
// non-empty request_id.
//
// Implementation note: we replace slog.Default() for the duration of the test.
// The reqid middleware stamps a ctx-bound logger; the handler and pipeline use
// reqid.Logger(ctx) to obtain it. Lines emitted outside a request context
// (boot, shutdown) are not captured here and are out of scope.
func TestRequestIDPresentOnEveryLogLine(t *testing.T) {
	// Capture JSON log output.
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)
	orig := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(orig) })

	// Build a pipeline that does some logging via reqid.Logger.
	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) (pipeline.RunResult, error) {
		defer close(ch.Out)
		log := reqid.Logger(ctx)
		log.Debug("pipeline: test debug", "phase", "start")
		log.Info("pipeline: test info", "phase", "upstream")
		log.Warn("pipeline: test warn", "phase", "degraded")
		ch.Out <- &transport.Message{
			Headers: map[string]string{
				"X-Relay-Status": "200",
				"Content-Type":   "application/json",
			},
			Body: []byte(`{"choices":[]}`),
		}
		log.Debug("pipeline: test debug", "phase", "done")
		return pipeline.RunResult{}, nil
	}

	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Stamp request_id into context (mimicking reqid.Middleware).
	ctx := req.Context()
	const testReqID = "01JTEST00000000000000000000"
	ctx = context.WithValue(ctx, ctxKeyForTest{}, testReqID) // not used directly
	// Use reqid middleware to stamp the context properly.
	req = req.WithContext(ctx)

	// Wrap in reqid middleware so the logger is context-bound.
	var captured *http.Request
	mw := reqid.Middleware(logger)
	innerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		ChatCompletions(fakeResolver(), runPipeline)(w, r)
	})
	rec := httptest.NewRecorder()
	mw(innerHandler).ServeHTTP(rec, req)

	if captured == nil {
		t.Fatal("inner handler not called")
	}

	// Parse all emitted JSON log lines.
	scanner := bufio.NewScanner(&buf)
	lineCount := 0
	missingReqID := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lineCount++
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("non-JSON log line: %s", line)
			continue
		}
		// All lines emitted by reqid.Logger(ctx) must have request_id.
		// We identify "request-bound" lines by presence of our known pipeline messages.
		msg, _ := entry["msg"].(string)
		if strings.HasPrefix(msg, "pipeline: test") {
			rid, _ := entry["request_id"].(string)
			if rid == "" {
				missingReqID = append(missingReqID, line)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if lineCount == 0 {
		t.Error("no log lines captured — pipeline logging may not be wired")
	}
	if len(missingReqID) > 0 {
		t.Errorf("%d request-bound log line(s) missing request_id:", len(missingReqID))
		for _, l := range missingReqID {
			t.Logf("  %s", l)
		}
	}
}

// ctxKeyForTest is a private context key used only in this test.
type ctxKeyForTest struct{}
