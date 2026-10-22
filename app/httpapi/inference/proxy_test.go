package inference

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/wyolet/relay/app/adapter"
	apphost "github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func TestProxyUpstreamPath_DefaultsToSpecPath(t *testing.T) {
	spec := (&adapter.Spec{DefaultPath: "/v1/responses"}).Build()

	got := proxyUpstreamPath("/openai/v1/responses", spec, nil)

	if got != "/v1/responses" {
		t.Fatalf("path: got %q", got)
	}
}

func TestProxyUpstreamPath_HostPathWins(t *testing.T) {
	spec := (&adapter.Spec{DefaultPath: "/v1/responses"}).Build()
	p := "/backend-api/codex/responses"
	host := &apphost.Host{Spec: apphost.Spec{Path: &p}}

	if got := proxyUpstreamPath("/openai/v1/responses", spec, host); got != p {
		t.Fatalf("path: got %q", got)
	}

	// Explicit "" is a real value: baseURL is the whole endpoint.
	empty := ""
	host.Spec.Path = &empty
	if got := proxyUpstreamPath("/openai/v1/responses", spec, host); got != "" {
		t.Fatalf("path: got %q, want empty", got)
	}
}

// proxyTestBody builds a JSON chat body around the given model with a
// padding payload, placing model first or last among the top-level keys.
func proxyTestBody(model string, padding int, modelLast bool) []byte {
	pad := strings.Repeat("a", padding)
	if modelLast {
		return []byte(`{"messages":[{"role":"user","content":"` + pad + `"}],"model":"` + model + `"}`)
	}
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"` + pad + `"}]}`)
}

// holdProxyBody reads only until the model is known, and the scannedBody then forwards exactly the client's bytes.
func TestHoldProxyBody(t *testing.T) {
	cases := []struct {
		name       string
		body       []byte
		src        func([]byte) io.Reader
		noModel    bool   // needModel false
		code       string // "" = held
		heldAll    bool   // the whole body was held
		heldAtMost int
		whole      bool
	}{
		{name: "small body", body: proxyTestBody("test-model", 64, false), heldAll: true},
		{name: "small body ending with its last read", body: proxyTestBody("test-model", 64, false), src: func(b []byte) io.Reader { return iotest.DataErrReader(bytes.NewReader(b)) }, heldAll: true, whole: true},
		{name: "model first streams the rest", body: proxyTestBody("test-model", 2<<20, false), heldAtMost: proxyHoldBytes},
		{name: "model first, one byte per read", body: proxyTestBody("test-model", 64<<10, false), src: func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) }, heldAtMost: 32},
		{name: "model last holds the body", body: proxyTestBody("test-model", 2<<20, true), heldAll: true},
		{name: "model past the buffer cap", body: proxyTestBody("test-model", proxyMaxBodyBuffer, true), code: "request_too_large"},
		{name: "not json", body: []byte("not-json"), code: "parse_error"},
		{name: "model not a string", body: []byte(`{"model":42}`), code: "parse_error"},
		{name: "missing model", body: []byte(`{"messages":[]}`), code: "missing_model"},
		{name: "empty model", body: []byte(`{"model":"","messages":[]}`), code: "missing_model"},
		{name: "duplicate in the held part", body: []byte(`{"model":"test-model","model":"other"}`), code: "duplicate_model"},
		{name: "client gone", body: proxyTestBody("test-model", 64, true), src: func(b []byte) io.Reader {
			return io.MultiReader(bytes.NewReader(b[:20]), iotest.ErrReader(io.ErrUnexpectedEOF))
		}, code: "read_body"},
		{name: "no model needed holds nothing", body: proxyTestBody("test-model", 2<<20, true), noModel: true},
		{name: "model key in other case", body: []byte(`{"Model":"test-model","messages":[]}`), code: "invalid_model_key"},
		{name: "escaped model key", body: []byte(`{"` + uEscape("006d") + `odel":"test-model","messages":[]}`), code: "invalid_model_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var src io.Reader = bytes.NewReader(tc.body)
			if tc.src != nil {
				src = tc.src(tc.body)
			}
			sb, held, rej := holdProxyBody(io.NopCloser(src), int64(len(tc.body)), !tc.noModel)
			if tc.code != "" {
				if rej == nil || rej.code != tc.code {
					t.Fatalf("got %v, want %s", rej, tc.code)
				}
				return
			}
			if rej != nil {
				t.Fatalf("hold: %v", rej)
			}
			if !bytes.HasPrefix(tc.body, held) {
				t.Fatal("held bytes are not a prefix of the body")
			}
			switch {
			case tc.noModel && len(held) != 0:
				t.Fatalf("held %d bytes with no model needed", len(held))
			case tc.heldAll && len(held) != len(tc.body):
				t.Fatalf("held %d of %d bytes", len(held), len(tc.body))
			case tc.heldAtMost > 0 && len(held) > tc.heldAtMost:
				t.Fatalf("held %d bytes, want at most %d", len(held), tc.heldAtMost)
			}
			if sb.whole != tc.whole {
				t.Fatalf("whole = %v, want %v", sb.whole, tc.whole)
			}
			if !tc.noModel && sb.scan.model != "test-model" {
				t.Fatalf("model %q", sb.scan.model)
			}
			got, err := io.ReadAll(sb)
			if err != nil || !bytes.Equal(got, tc.body) {
				t.Fatalf("forwarded %d bytes (%v), want the body's %d", len(got), err, len(tc.body))
			}
		})
	}
}

// A declared length sizes the hold: a small body in one exact allocation, a larger one with its model last in one more, not a doubling series.
func TestHoldProxyBody_SizesTheBufferFromContentLength(t *testing.T) {
	body := proxyTestBody("test-model", 1000, true)
	_, held, rej := holdProxyBody(io.NopCloser(bytes.NewReader(body)), int64(len(body)), true)
	if rej != nil || cap(held) != len(body) {
		t.Fatalf("held cap %d for a %d-byte body (%v)", cap(held), len(body), rej)
	}

	body = proxyTestBody("test-model", 300<<10, true)
	_, held, rej = holdProxyBody(io.NopCloser(bytes.NewReader(body)), int64(len(body)), true)
	if rej != nil || len(held) != len(body) || cap(held)-len(body) > 16<<10 {
		t.Fatalf("held %d bytes in cap %d for a %d-byte body (%v)", len(held), cap(held), len(body), rej)
	}

	// An undeclared length cannot reserve memory: the buffer doubles from the first read's size.
	if got := holdCapacity(proxyHoldBytes, -1); got != 2*proxyHoldBytes {
		t.Fatalf("undeclared growth %d", got)
	}
	if got := holdCapacity(proxyHoldBytes, 30<<20); got != maxUpfrontBodyBytes {
		t.Fatalf("a large declared length grew the buffer to %d at once, want the upfront cap %d", got, maxUpfrontBodyBytes)
	}
}

// TestBodyCapture_ConcurrentFinalize races finalizeInto against a
// transport-like reader still draining the tee: the published snapshot
// must always be a consistent prefix of the body, stable after finalize,
// and flagged truncated whenever it is partial. Meaningful under -race.
func TestBodyCapture_ConcurrentFinalize(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	prefix := append([]byte(nil), body[:1024]...)
	c := newBodyCapture(prefix, len(body))
	tee := c.tee(bytes.NewReader(body[1024:]))

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, tee)
		close(done)
	}()

	lc := lifecycle.NewContext("req-race", "proxy", time.Now())
	c.finalizeInto(lc)
	if !bytes.Equal(lc.RequestBody, body[:len(lc.RequestBody)]) {
		t.Fatal("published capture is not a prefix of the body")
	}
	if len(lc.RequestBody) < len(body) && !lc.RequestBodyTruncated {
		t.Fatal("partial capture must be flagged truncated")
	}
	snap := append([]byte(nil), lc.RequestBody...)
	<-done
	if !bytes.Equal(lc.RequestBody, snap) {
		t.Fatal("published capture mutated after finalize")
	}
}

// The proxy path's rate-limit rejection is the same condition as the pipeline
// path's, and must not report a different error.type/code for it.
func TestMapProxyErr_RateLimit_UsesTheSharedEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	mapProxyErr(rec, &pkgratelimit.KeyQuotaExhausted{
		Rule:       pkgratelimit.Rule{Name: "rpm"},
		RetryAfter: 2300 * time.Millisecond,
	})

	if rec.Code != 429 {
		t.Fatalf("status: %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After: %q, want %q", got, "3")
	}
	var env httpapi.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Err.Type != "rate_limit_error" || env.Err.Code != "rate_limit_exceeded" {
		t.Fatalf("envelope: type=%q code=%q", env.Err.Type, env.Err.Code)
	}
	if !strings.Contains(env.Err.Message, "try again in 3s") {
		t.Fatalf("message must name the retry delay: %q", env.Err.Message)
	}
}
