package inference

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapters"
)

// A host that refuses connections is reported to the caller by slug; its base URL and dial target stay in the logs.
func TestDispatch_UnreachableHostKeepsItsAddressOutOfTheResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := up.URL
	up.Close()
	addr := strings.TrimPrefix(closed, "http://")

	cat, pr := buildDispatchCatalog(t, "internal-host", adapters.OpenAI)
	pointHostAt(t, cat, closed)
	d := buildRunnableDeps(t, cat)

	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil), pr)
	w := httptest.NewRecorder()
	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`),
		ModelName: "test-model",
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if strings.Contains(body, addr) || strings.Contains(body, "/v1/chat/completions") {
		t.Fatalf("response discloses the upstream address: %s", body)
	}
	e := parseDispatchErr(t, w.Body.Bytes())
	if e.Error.Code != "upstream_unreachable" || !strings.Contains(e.Error.Message, "internal-host") {
		t.Errorf("error = %+v, want upstream_unreachable naming the host slug", e.Error)
	}
}

// Other transport failures (TLS, timeouts) carry the request URL in Go's error text; neither error mapper may pass it through.
func TestErrorMappers_RedactTransportURLs(t *testing.T) {
	transportErr := &url.Error{Op: "Post", URL: "https://10.0.0.7:8443/v1/messages", Err: errors.New("tls: failed to verify certificate")}

	for name, write := range map[string]func(http.ResponseWriter){
		"pipeline": func(w http.ResponseWriter) { mapPipelineErr(w, transportErr) },
		"proxy":    func(w http.ResponseWriter) { mapProxyErr(w, transportErr) },
	} {
		w := httptest.NewRecorder()
		write(w)
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d, want 502", name, w.Code)
		}
		if body := w.Body.String(); strings.Contains(body, "10.0.0.7") || strings.Contains(body, "/v1/messages") {
			t.Errorf("%s: response discloses the upstream address: %s", name, body)
		}
	}
}
