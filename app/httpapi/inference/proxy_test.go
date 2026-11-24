package inference

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/pkg/adapters"
	apphost "github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// The host's own path wins verbatim (an explicit "" means the base URL is the whole endpoint), then the shape's native path, then the inbound path.
func TestProxyUpstreamPath(t *testing.T) {
	spec := (&adapter.Spec{DefaultPath: "/v1/responses"}).Build()
	own, empty := "/backend-api/codex/responses", ""
	for _, tc := range []struct {
		name string
		spec *adapter.Spec
		host *apphost.Host
		want string
	}{
		{"shape's native path", spec, &apphost.Host{}, "/v1/responses"},
		{"no host", spec, nil, "/v1/responses"},
		{"host path", spec, &apphost.Host{Spec: apphost.Spec{Path: &own}}, own},
		{"host path for the shape", (&adapter.Spec{Name: "openai_responses", DefaultPath: "/v1/responses"}).Build(), &apphost.Host{Spec: apphost.Spec{Path: &empty, Paths: map[adapters.Name]string{"openai_responses": own}}}, own},
		{"empty host path", spec, &apphost.Host{Spec: apphost.Spec{Path: &empty}}, ""},
		{"unknown shape", nil, &apphost.Host{}, "/openai/v1/responses"},
		{"shape without a native path", &adapter.Spec{}, nil, "/openai/v1/responses"},
	} {
		if got, err := proxyUpstreamPath("/openai/v1/responses", tc.spec, tc.host); got != tc.want || err != nil {
			t.Errorf("%s: path %q, %v; want %q", tc.name, got, err, tc.want)
		}
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

// The capture is the held prefix plus what streamed past, up to its limit; it is whole only when the body ended within the limit before the capture was published.
func TestBodyCapture(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limit     int
		read      int // reads of the rest before the capture is published; -1 = to the end
		want      string
		truncated bool
	}{
		{"whole body", 64, -1, "prefix-0123456789", false},
		{"body ending exactly at the limit", 17, -1, "prefix-0123456789", false},
		{"limit inside a read", 12, -1, "prefix-01234", true},
		{"limit inside the prefix", 3, -1, "prefix-", true},
		{"published before the body ended", 64, 1, "prefix-0123", true},
		{"published before any read", 64, 0, "prefix-", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newBodyCapture([]byte("prefix-"), tc.limit)
			// Four bytes a read, the last one returning its data with the end.
			tee := c.tee(iotest.DataErrReader(strings.NewReader("0123456789")))
			buf := make([]byte, 4)
			for n := 0; tc.read < 0 || n < tc.read; n++ {
				if _, err := tee.Read(buf); err != nil {
					break
				}
			}
			lc := lifecycle.NewContext("r", "proxy", time.Now())
			c.finalizeInto(lc)
			_, _ = io.Copy(io.Discard, tee)
			if string(lc.RequestBody) != tc.want || lc.RequestBodyTruncated != tc.truncated {
				t.Fatalf("captured %q (truncated %v), want %q (truncated %v)", lc.RequestBody, lc.RequestBodyTruncated, tc.want, tc.truncated)
			}
		})
	}
}

// Racing finalizeInto against a reader still draining the tee, the published capture is always a prefix of the body, flagged truncated when partial, and stable afterwards. Meaningful under -race.
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

// A proxy request that names no usable upstream is refused before anything is forwarded, on a connection that then closes, and recorded with the reason.
func TestProxyShape_RoutingRefusals(t *testing.T) {
	invalid := func(code, msg string) httpapi.APIErrorBody {
		return httpapi.APIErrorBody{Type: "invalid_request_error", Code: code, Message: msg}
	}
	for _, tc := range []struct {
		name      string
		anonymous bool
		noPlans   bool
		pin       string
		model     string
		status    int
		want      httpapi.APIErrorBody
		recorded  outcome // Status is the upstream's: 0 when relay answered without routing
	}{
		{name: "unknown pinned host", pin: "nowhere", model: "test-model", status: 400,
			want: invalid("unknown_upstream_host", `unknown upstream host "nowhere"; see GET /v1/proxy/hosts`), recorded: outcome{Kind: "unknown_upstream_host", Message: "unknown upstream host nowhere"}},
		{name: "unknown pinned host, anonymous", anonymous: true, pin: "nowhere", model: "test-model", status: 400,
			want: invalid("unknown_upstream_host", `unknown upstream host "nowhere"; see GET /v1/proxy/hosts`), recorded: outcome{Kind: "unknown_upstream_host", Message: "unknown upstream host nowhere"}},
		{name: "anonymous without a pin", anonymous: true, model: "test-model", status: 400,
			want:     invalid("missing_upstream_host", "anonymous proxy traffic requires X-WR-Upstream-Host header naming a configured host"),
			recorded: outcome{Kind: "missing_upstream_host", Message: "anonymous proxy traffic requires an upstream-host header"}},
		{name: "anonymous before the plans are published", anonymous: true, noPlans: true, pin: "anthropic", model: "test-model", status: 503,
			want: httpapi.APIErrorBody{Type: "server_error", Code: "not_ready", Message: "relay is starting; retry shortly"}, recorded: outcome{Kind: "not_ready", Message: "plans not published yet"}},
		{name: "model the caller cannot route", model: "no-such-model", status: 404,
			want:     httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_not_found", Refusal: "host_not_reachable", Message: `model "no-such-model" not found`},
			recorded: outcome{Status: 404, Kind: "model_not_found", Refusal: "host_not_reachable", Message: `model "no-such-model" not found`}},
		{name: "model not served at the pinned host", pin: "anthropic", model: "no-such-model", status: 404,
			want:     httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_not_found", Refusal: "host_not_reachable", Message: `model "no-such-model" not found`},
			recorded: outcome{Status: 404, Kind: "model_not_found", Refusal: "host_not_reachable", Message: `model "no-such-model" not found`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, got := recordingUpstream(t)
			d, pr, _ := proxyStreamDeps(t, up.URL)
			outcomes := watchOutcomes(&d)
			if tc.noPlans {
				d.Plans = nil
			}
			call := proxyShapeCall{principal: pr, pin: tc.pin, body: bytes.NewReader(proxyTestBody(tc.model, 16, false)), length: -1}
			if tc.anonymous {
				call.principal = nil
			}
			wantAnswer(t, serveProxyShape(d, call), tc.status, tc.want, map[string]string{"Connection": "close"})
			wantOutcome(t, outcomes, tc.recorded)
			if len(got) != 0 {
				t.Error("the upstream was reached")
			}
		})
	}
}

// A caller that holds the whole body (a WebSocket frame) hands it to Dispatch without a scannedBody: it is scanned as proxy mode scans a streamed one, then forwarded unchanged.
func TestDispatch_ProxyWholeBody(t *testing.T) {
	up, got := recordingUpstream(t)
	d, pr, _ := proxyStreamDeps(t, up.URL)
	seen := watchPostFlight(&d)
	d.Lifecycle = d.Proxy.Lifecycle
	run := func(body []byte) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		Dispatch(d, w, proxyShapeCall{principal: pr}.request(d), DispatchInput{Inbound: adapters.Anthropic, Body: body, ModelName: "test-model"})
		return w
	}

	body := proxyTestBody("test-model", 64, false)
	if w := run(body); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if g := <-got; !bytes.Equal(g.body, body) {
		t.Errorf("upstream got %q, want the body as sent", g.body)
	}
	if s := awaitPostFlight(t, seen); s.requestBytes != len(body) || s.errorKind != "" {
		t.Errorf("post-flight saw %d bytes, error %q; want %d and none", s.requestBytes, s.errorKind, len(body))
	}

	for body, want := range map[string]httpapi.APIErrorBody{
		`{"model":"test-model","model":"other"}`: {Type: "invalid_request_error", Code: "duplicate_model", Message: "request body has more than one top-level 'model' field"},
		`{"Model":"test-model"}`: {Type: "invalid_request_error", Code: "invalid_model_key",
			Message: `proxy mode forwards the body as sent: its top-level 'model' key must be spelled exactly "model", without escapes or other letter case`},
	} {
		wantAnswer(t, run([]byte(body)), http.StatusBadRequest, want, nil)
		if s := awaitPostFlight(t, seen); s.errorKind != want.Code {
			t.Errorf("%s: recorded as %q, want %s", body, s.errorKind, want.Code)
		}
	}
	if len(got) != 0 {
		t.Error("a refused body reached the upstream")
	}
}

// Every failure of the proxy run is answered in relay's envelope; a refused counter and an unreachable store answer as they do on the pipeline path.
func TestMapProxyErr(t *testing.T) {
	c := limits.Counter{Kind: limits.KindRule, Rung: limits.RungSystem, ID: "r:x"}
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		want    httpapi.APIErrorBody
		headers map[string]string
	}{
		{"refused body", fmt.Errorf("forward: %w", errTrailingBytes), 400,
			httpapi.APIErrorBody{Type: "invalid_request_error", Code: "parse_error", Message: "invalid JSON: more than 1024 bytes after the top-level value"}, nil},
		{"stalled forward", errForwardStalled, 502,
			httpapi.APIErrorBody{Type: "upstream_error", Code: "upstream_stalled", Message: "the request body stopped moving: the upstream stopped reading it, or the client stopped sending"}, nil},
		{"no upstream credential", proxy.ErrNoUpstreamAuth, 400,
			httpapi.APIErrorBody{Type: "invalid_request_error", Code: "missing_upstream_auth", Message: "proxy mode requires Authorization: Bearer <upstream-key>"}, nil},
		{"no limiter", proxy.ErrLimiterMissing, 500,
			httpapi.APIErrorBody{Type: "server_error", Code: "limiter_missing", Message: "proxy mode is not wired to a limiter"}, nil},
		{"state store unreachable", fmt.Errorf("reserve: %w", pkgratelimit.ErrUnavailable), 503,
			httpapi.APIErrorBody{Type: "server_error", Code: "overloaded", Message: httpapi.OverloadMessage}, map[string]string{"Retry-After": httpapi.RetryAfterShed}},
		{"refused counter", &limits.Refused{Counter: &c, RetryAfter: 2300 * time.Millisecond}, 429,
			httpapi.APIErrorBody{Type: "rate_limit_error", Code: "rate_limit_exceeded", Refusal: "no_capacity", Message: "the rate limit of the system policy  is exhausted; try again in 3s"},
			map[string]string{"Retry-After": "3"}},
		{"upstream failure", errors.New("unexpected EOF"), 502,
			httpapi.APIErrorBody{Type: "server_error", Code: "upstream_error", Message: "upstream: unexpected EOF"}, nil},
		{"upstream failure naming an address", errors.New("dial tcp 10.0.0.7:443: connection refused"), 502,
			httpapi.APIErrorBody{Type: "server_error", Code: "upstream_error", Message: "upstream: upstream connection failed"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mapProxyErr(rec, nil, tc.err)
			wantAnswer(t, rec, tc.status, tc.want, tc.headers)
			if got := rec.Header().Get(HeaderOrigin); got != "relay" {
				t.Errorf("%s = %q, want relay", HeaderOrigin, got)
			}
		})
	}

	rec := httptest.NewRecorder()
	mapProxyErr(rec, &adapter.Spec{Overloaded: testOverloaded}, pkgratelimit.ErrUnavailable)
	assertSpecOverloaded(t, rec)
}
