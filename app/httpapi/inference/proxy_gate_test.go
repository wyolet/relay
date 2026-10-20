package inference

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/httpheader"
)

// poisonBody fails the test when read: a refused request must cost no body read.
type poisonBody struct{ t *testing.T }

func (b poisonBody) Read([]byte) (int, error) {
	b.t.Error("request body was read")
	return 0, io.ErrUnexpectedEOF
}

// poisonedProxyRequest is a proxy-mode request whose body must never be read; an empty relayKey makes it anonymous.
func poisonedProxyRequest(t *testing.T, relayKey string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)
	r.Body = io.NopCloser(poisonBody{t})
	r.Header.Set(httpheader.HeaderProxyMode, httpheader.ProxyModeValueProxy)
	r.Header.Set("Authorization", "Bearer upstream-key")
	if relayKey != "" {
		r.Header.Set(httpheader.HeaderRelayAPIKey, relayKey)
	}
	return r
}

// gatedEdge is captureEdge recording whether the request got past the middleware.
func gatedEdge(d Deps, reached *bool) http.Handler {
	spec := d.Specs.Spec(adapters.OpenAI)
	return ClassifyMiddleware()(PrincipalMiddleware(d.Catalog, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		handleShape(spec, d, w, r)
	})))
}

// The proxy-mode setting ships disabled, so both proxy modes are refused at the credential middleware with the envelope the proxy handler used to answer.
func TestProxyModeDisabledIsRefusedBeforeTheBody(t *testing.T) {
	const want = `{"error":{"type":"invalid_request_error","code":"proxy_disabled","message":"proxy mode is not enabled on this relay"}}`
	on := true
	for _, tc := range []struct {
		name     string
		relayKey string
	}{
		{"anonymous", ""},
		{"authed passthrough key", captureKeyPlaintext},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, events := captureDeps(t, captureCatalog(t, okUpstream(t).URL, captureCase{policyCaptures: &on}))
			before := rejectionCount(t, "proxy_disabled")

			var reached bool
			w := httptest.NewRecorder()
			gatedEdge(d, &reached).ServeHTTP(w, poisonedProxyRequest(t, tc.relayKey))

			if w.Code != http.StatusForbidden || w.Body.String() != want {
				t.Fatalf("got %d %s, want 403 %s", w.Code, w.Body, want)
			}
			wantHeader := http.Header{}
			wantHeader.Set("Content-Type", "application/json")
			wantHeader.Set(HeaderOrigin, "relay")
			wantHeader.Set(httpheader.HeaderShouldRetry, "false")
			if !maps.EqualFunc(w.Header(), wantHeader, slices.Equal[[]string]) {
				t.Fatalf("headers = %v, want %v", w.Header(), wantHeader)
			}
			if reached {
				t.Fatal("the refused request reached the handler")
			}
			if got := rejectionCount(t, "proxy_disabled") - before; got != 1 {
				t.Fatalf("counter rose by %v, want 1", got)
			}
			assertNoPostFlight(t, events)
		})
	}
}

// The credential is judged before the setting, so a key that may not bring its own upstream key keeps its own answer.
func TestProxyModeRefusalComesAfterTheCredential(t *testing.T) {
	on := true
	cat := captureCatalog(t, okUpstream(t).URL, captureCase{policyCaptures: &on})
	k, _ := cat.Current().KeyByHash(sha(captureKeyPlaintext))
	noPassthrough := *k
	noPassthrough.Spec.PassthroughAllowed = false
	if err := cat.ApplyKeyUpsert(&noPassthrough); err != nil {
		t.Fatalf("key upsert: %v", err)
	}
	d, events := captureDeps(t, cat)

	for _, tc := range []struct {
		relayKey string
		status   int
		code     string
	}{
		{"sk-wr-unknown", http.StatusUnauthorized, "unauthenticated"},
		{captureKeyPlaintext, http.StatusForbidden, "passthrough_forbidden"},
	} {
		var reached bool
		w := httptest.NewRecorder()
		gatedEdge(d, &reached).ServeHTTP(w, poisonedProxyRequest(t, tc.relayKey))
		if w.Code != tc.status || errCode(t, w.Body.Bytes()) != tc.code {
			t.Fatalf("%s: got %d %s, want %d %s", tc.relayKey, w.Code, w.Body, tc.status, tc.code)
		}
	}
	assertNoPostFlight(t, events)
}

func TestProxyModeRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pm     *settings.ProxyMode
		mode   Mode
		status int
		code   string
	}{
		{"no setting", nil, ModeProxyAuthed, http.StatusForbidden, "proxy_disabled"},
		{"disabled, authed", &settings.ProxyMode{}, ModeProxyAuthed, http.StatusForbidden, "proxy_disabled"},
		{"disabled, anonymous allowed", &settings.ProxyMode{AllowUnauthenticated: true}, ModeProxyAnonymous, http.StatusForbidden, "proxy_disabled"},
		{"enabled, authed", &settings.ProxyMode{Enabled: true}, ModeProxyAuthed, 0, ""},
		{"enabled, anonymous not allowed", &settings.ProxyMode{Enabled: true}, ModeProxyAnonymous, http.StatusUnauthorized, "unauthenticated"},
		{"enabled, anonymous allowed", &settings.ProxyMode{Enabled: true, AllowUnauthenticated: true}, ModeProxyAnonymous, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, code, _ := proxyModeRefusal(tc.pm, tc.mode)
			if status != tc.status || code != tc.code {
				t.Fatalf("got %d %q, want %d %q", status, code, tc.status, tc.code)
			}
		})
	}
}

// assertNoPostFlight fails if a post-flight event fires: the hook runs detached, so silence over a short window is the evidence.
func assertNoPostFlight(t *testing.T, events <-chan bool) {
	t.Helper()
	select {
	case <-events:
		t.Fatal("a refused request fired a post-flight event")
	case <-time.After(100 * time.Millisecond):
	}
}
