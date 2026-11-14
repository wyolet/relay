package inference

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/httpapi"
)

func classifyRequest(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.5:54321"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestClassify(t *testing.T) {
	// No trusted proxies are configured under test, so X-Forwarded-For is ignored and the peer address is the client's.
	const ip = "203.0.113.5"
	tests := []struct {
		name    string
		headers map[string]string
		want    Classification
		wantErr error
	}{
		{"bearer key", map[string]string{"Authorization": "Bearer wr_abc", "X-Forwarded-For": "10.0.0.1"},
			Classification{Mode: ModeNormal, Key: "wr_abc", ClientIP: ip}, nil},
		{"X-WR-API-Key wins over the bearer", map[string]string{"X-WR-API-Key": "wr_via_header", "Authorization": "Bearer wr_via_auth", "X-Api-Key": "wr_via_x"},
			Classification{Mode: ModeNormal, Key: "wr_via_header", ClientIP: ip}, nil},
		{"the bearer wins over x-api-key", map[string]string{"Authorization": "Bearer wr_via_auth", "X-Api-Key": "wr_via_x"},
			Classification{Mode: ModeNormal, Key: "wr_via_auth", ClientIP: ip}, nil},
		{"x-api-key alone", map[string]string{"X-Api-Key": "wr_via_x"},
			Classification{Mode: ModeNormal, Key: "wr_via_x", ClientIP: ip}, nil},
		{"an Authorization that is not a bearer falls through", map[string]string{"Authorization": "Basic abc", "X-Api-Key": "wr_via_x"},
			Classification{Mode: ModeNormal, Key: "wr_via_x", ClientIP: ip}, nil},
		{"no credential", nil, Classification{Mode: ModeNormal, ClientIP: ip}, nil},
		{"explicit No-Proxy", map[string]string{"X-WR-Proxy-Mode": "No-Proxy", "Authorization": "Bearer wr_abc", "X-WR-Upstream-Host": "openai"},
			Classification{Mode: ModeNormal, Key: "wr_abc", ClientIP: ip}, nil},
		{"proxy with a relay key", map[string]string{"X-WR-Proxy-Mode": "Proxy", "X-WR-API-Key": "wr_relay", "X-WR-Upstream-Host": "anthropic", "Authorization": "Bearer sk-ant-oauth-token"},
			Classification{Mode: ModeProxyAuthed, Key: "wr_relay", UpstreamAuth: "Bearer sk-ant-oauth-token", UpstreamHost: "anthropic", ClientIP: ip}, nil},
		// In proxy mode x-api-key is not a relay credential: only X-WR-API-Key is.
		{"proxy without a relay key", map[string]string{"X-WR-Proxy-Mode": "Proxy", "X-WR-Upstream-Host": "openai", "Authorization": "Bearer sk-up", "X-Api-Key": "wr_via_x"},
			Classification{Mode: ModeProxyAnonymous, UpstreamAuth: "Bearer sk-up", UpstreamHost: "openai", ClientIP: ip}, nil},
		{"proxy without an upstream key", map[string]string{"X-WR-Proxy-Mode": "Proxy", "X-WR-Upstream-Host": "anthropic", "X-WR-API-Key": "wr_relay"},
			Classification{}, ErrMissingUpstreamKey},
		{"unknown proxy mode", map[string]string{"X-WR-Proxy-Mode": "weird"}, Classification{}, ErrInvalidProxyMode},
		{"proxy mode is case-sensitive", map[string]string{"X-WR-Proxy-Mode": "proxy"}, Classification{}, ErrInvalidProxyMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(classifyRequest(tt.headers))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("classification (-want +got):\n%s", diff)
			}
		})
	}
}

// The middleware hands the classification on, and answers a request it cannot classify itself.
func TestClassifyMiddleware(t *testing.T) {
	var seen Classification
	h := ClassifyMiddleware()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = ClassificationFrom(r.Context()) }))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, classifyRequest(map[string]string{"Authorization": "Bearer wr_abc"}))
	if diff := cmp.Diff(Classification{Mode: ModeNormal, Key: "wr_abc", ClientIP: "203.0.113.5"}, seen); diff != "" || w.Code != http.StatusOK {
		t.Errorf("status %d; classification on the context (-want +got):\n%s", w.Code, diff)
	}

	seen = Classification{}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, classifyRequest(map[string]string{"X-WR-Proxy-Mode": "Proxy"}))
	wantAnswer(t, w, http.StatusBadRequest, httpapi.APIErrorBody{
		Type: "invalid_request_error", Code: "bad_request", Message: "inference: proxy mode requires Authorization: Bearer <upstream-key>",
	}, nil)
	if seen != (Classification{}) {
		t.Error("an unclassifiable request reached the handler")
	}
	if got := ClassificationFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != (Classification{}) {
		t.Errorf("classification without the middleware = %+v, want zero", got)
	}
}
