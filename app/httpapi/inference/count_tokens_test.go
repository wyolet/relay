package inference

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/tokencount"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/kv"
)

// countProfile is stubProfile (anthropic shape, no User-Agent match) plus the count-tokens route, a session marker and a "counting/" model prefix it strips.
type countProfile struct{ stubProfile }

func (countProfile) Name() string                    { return "counting" }
func (countProfile) TokenCountPath() string          { return "/v1/messages/count_tokens" }
func (countProfile) SessionKey(h http.Header) string { return h.Get("x-test-session") }
func (countProfile) Inbound(model string) string     { return strings.TrimPrefix(model, "counting/") }

func profileRegistry(p ...clientprofile.Profile) *clientprofile.Registry {
	reg := clientprofile.New()
	for _, one := range p {
		_ = reg.Register(one)
	}
	return reg
}

// countingRegistry mirrors buildTestRegistry's anthropic spec with a CountPath, so its pipeline adapter carries the upstream-counter capability.
func countingRegistry() *adapter.Registry {
	return adapter.NewRegistry((&adapter.Spec{
		Name:        adapters.Anthropic,
		DefaultPath: "/v1/messages",
		CountPath:   "/v1/messages/count_tokens",
		Auth:        adapter.AuthStrategy{Header: "x-api-key"},
		Translator:  stubV1Translator{},
	}).Build())
}

// countCatalog points the fixture host at upstreamURL, so a count reaches the test server.
func countCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *Principal) {
	t.Helper()
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	pointHostAt(t, cat, upstreamURL)
	return cat, rk
}

// countRequest drives the handler the way the mounted route does: classified, authed as rk (nil sends no credential), with the profile on the context.
func countRequest(d Deps, rk *Principal, body io.Reader, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/counting/v1/messages/count_tokens", body)
	for k, vs := range headers {
		r.Header[k] = vs
	}
	if rk != nil {
		r = withNormalContext(r, rk)
	}
	r = r.WithContext(clientprofile.WithProfile(r.Context(), countProfile{}))
	w := httptest.NewRecorder()
	handleCountTokens(d, w, r)
	return w
}

// wantTokenCount checks a count answered whole: the client's body, and the tier that produced it in the header.
func wantTokenCount(t *testing.T, w *httptest.ResponseRecorder, count string, tier string) {
	t.Helper()
	if w.Code != http.StatusOK || w.Body.String() != `{"input_tokens":`+count+`}` {
		t.Errorf("answer = %d %s, want 200 with %s input tokens", w.Code, w.Body, count)
	}
	want := http.Header{"Content-Type": {"application/json"}, httpheader.HeaderTokenCount: {tier}}
	if diff := cmp.Diff(want, w.Header()); diff != "" {
		t.Errorf("headers (-want +got):\n%s", diff)
	}
}

const countBody = `{"model":"test-model","messages":[]}` // 36 bytes

// The count comes from the best source the route affords: the upstream's own counter, else the ratio relay measured on comparable traffic, else bytes alone.
func TestCountTokens_Tiers(t *testing.T) {
	session := http.Header{"X-Test-Session": {"sess-1"}}
	for _, tc := range []struct {
		name     string
		upstream string // the upstream counter's reply; "" = the shape declares no counter
		observed bool   // ratios were measured: 0.5 tokens a byte for the model, 0.25 in session sess-1
		body     string
		header   http.Header
		count    string
		tier     string
	}{
		{name: "upstream counter", upstream: `{"input_tokens":777}`, observed: true, body: countBody, count: "777", tier: "exact"},
		{name: "upstream counter, profile model prefix", upstream: `{"input_tokens":777}`, body: `{"model":"counting/test-model","messages":[]}`, count: "777", tier: "exact"},
		{name: "model ratio", observed: true, body: countBody, count: "18", tier: "calibrated"},
		{name: "session ratio wins over the model's", observed: true, body: countBody, header: session, count: "9", tier: "calibrated"},
		{name: "unknown session falls back to the model ratio", observed: true, body: countBody, header: http.Header{"X-Test-Session": {"sess-2"}}, count: "18", tier: "calibrated"},
		{name: "nothing known", body: countBody, header: session, count: "9", tier: "estimated"},
		{name: "host named by header", body: countBody, header: http.Header{httpheader.HeaderUpstreamHost: {"anthropic"}}, count: "9", tier: "estimated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, last := "http://upstream.invalid", func() (string, string) { return "", "" }
			if tc.upstream != "" {
				url, last = answeringUpstream(t, "application/json", tc.upstream)
			}
			cat, rk := countCatalog(t, url)
			d := buildRunnableDeps(t, cat)
			if tc.upstream != "" {
				useSpecs(&d, countingRegistry())
			}
			if tc.observed {
				mem := kv.NewMem()
				t.Cleanup(func() { _ = mem.Close() })
				d.TokenCalibrator = tokencount.NewCalibrator(mem)
				modelID := cat.Current().ModelsByName("test-model")[0].Meta.ID
				// A session's observation also moves its model's ratio, so the model-wide one goes last.
				d.TokenCalibrator.Observe(t.Context(), "sess-1", modelID, 1000, 250)
				d.TokenCalibrator.Observe(t.Context(), "", modelID, 1000, 500)
			}

			wantTokenCount(t, countRequest(d, rk, strings.NewReader(tc.body), tc.header), tc.count, tc.tier)
			path, body := last()
			if tc.upstream == "" {
				return
			}
			// The upstream is sent the caller's body with the model as the binding names it.
			if path != "/v1/messages/count_tokens" || body != countBody {
				t.Errorf("upstream got %s %s, want /v1/messages/count_tokens %s", path, body, countBody)
			}
		})
	}
}

func TestCountTokens_Refusals(t *testing.T) {
	invalid := func(code, msg string) httpapi.APIErrorBody {
		return httpapi.APIErrorBody{Type: "invalid_request_error", Code: code, Message: msg}
	}
	for _, tc := range []struct {
		name      string
		upstream  string
		body      io.Reader
		header    http.Header
		anonymous bool
		status    int
		want      httpapi.APIErrorBody
	}{
		{name: "body read fails", body: iotest.ErrReader(io.ErrUnexpectedEOF), status: 400, want: invalid("read_body", "unexpected EOF")},
		{name: "body over the limit", body: http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(strings.NewReader(countBody)), 8), status: 413,
			want: invalid("request_too_large", "http: request body too large")},
		{name: "not json", body: strings.NewReader(`{"model":`), status: 400, want: invalid("parse_error", "invalid JSON: unexpected end of JSON input")},
		{name: "duplicate model", body: strings.NewReader(duplicateModelBody), status: 400, want: invalid("duplicate_model", "request body has more than one top-level 'model' field")},
		{name: "no model", body: strings.NewReader(`{"messages":[]}`), status: 400, want: invalid("missing_model", "field 'model' is required")},
		{name: "no credential", body: strings.NewReader(countBody), anonymous: true, status: 401, want: invalid("unauthenticated", "missing relay key")},
		// A model the key cannot route fails as it does on the messages endpoint.
		{name: "unknown model", body: strings.NewReader(`{"model":"no-such-model"}`), status: 404,
			want: httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_not_found", Refusal: "host_not_reachable", Message: `model "no-such-model" not found`}},
		{name: "host header names another host", body: strings.NewReader(countBody), header: http.Header{httpheader.HeaderUpstreamHost: {"elsewhere"}}, status: 404,
			want: httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_not_found", Refusal: "host_not_reachable", Message: `model "test-model@elsewhere" not found`}},
		{name: "upstream counts nothing", upstream: `{"input_tokens":0}`, body: strings.NewReader(countBody), status: 502,
			want: httpapi.APIErrorBody{Type: "upstream_error", Code: "count_tokens_failed", Message: "adapter: count_tokens response carried no input_tokens"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := "http://upstream.invalid"
			if tc.upstream != "" {
				url, _ = answeringUpstream(t, "application/json", tc.upstream)
			}
			cat, rk := countCatalog(t, url)
			d := buildRunnableDeps(t, cat)
			if tc.upstream != "" {
				useSpecs(&d, countingRegistry())
			}
			if tc.anonymous {
				rk = nil
			}
			w := countRequest(d, rk, tc.body, tc.header)
			wantAnswer(t, w, tc.status, tc.want, map[string]string{httpheader.HeaderTokenCount: ""})
		})
	}
}

// An upstream token count spends a credential, so it holds the caller's counters like a generation: the second count under a one-request budget is refused before the upstream.
func TestCountTokens_UpstreamCountIsMetered(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer up.Close()
	cat, rk := countCatalog(t, up.URL)
	limitCaller(t, cat, 1)
	d := buildRunnableDeps(t, cat)
	useSpecs(&d, countingRegistry())

	wantTokenCount(t, countRequest(d, rk, strings.NewReader(countBody), nil), "7", "exact")
	w := countRequest(d, rk, strings.NewReader(countBody), nil)
	if w.Code != http.StatusTooManyRequests || envelopeOf(t, w).Code != "rate_limit_exceeded" {
		t.Fatalf("second count = %d %s, want 429 rate_limit_exceeded", w.Code, w.Body)
	}
	if hits != 1 {
		t.Fatalf("upstream counted %d times, want 1", hits)
	}
}

// The route is mounted only for a profile that declares it, behind the auth chain of /v1/*.
func TestMount_CountTokensRoute(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	d.Pinger = stubPinger{}
	d.Profiles = profileRegistry(countProfile{}, stubProfile{})

	r := chi.NewRouter()
	Mount(r, d)

	post := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(countBody))))
		return rec
	}
	wantUnauthenticated(t, post("/counting/v1/messages/count_tokens"), "missing relay key")
	if rec := post("/stub/v1/messages/count_tokens"); rec.Code != http.StatusNotFound {
		t.Errorf("a profile that declares no count route answered %d there, want 404", rec.Code)
	}
}
