package inference

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/adapters"
)

// A host that refuses connections is reported to the caller by slug; its base URL and dial target stay in the logs.
func TestDispatch_UnreachableHostKeepsItsAddressOutOfTheResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := up.URL
	up.Close()

	cat, pr := buildDispatchCatalog(t, "internal-host", adapters.OpenAI)
	pointHostAt(t, cat, closed)
	d := buildRunnableDeps(t, cat)

	w := dispatch(d, pr, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`),
		ModelName: "test-model",
	})
	wantAnswer(t, w, http.StatusBadGateway, httpapi.APIErrorBody{
		Type: "server_error", Code: "upstream_unreachable", Refusal: "host_not_reachable",
		Message: `upstream host "internal-host" unreachable after 3 attempt(s)`,
	}, map[string]string{HeaderOrigin: "relay"})
}

// Other transport failures (TLS, timeouts) carry the request URL in Go's error text; the proxy's error mapper may not pass it through either.
func TestMapProxyErr_RedactsTransportURLs(t *testing.T) {
	w := httptest.NewRecorder()
	mapProxyErr(w, nil, &url.Error{Op: "Post", URL: "https://10.0.0.7:8443/v1/messages", Err: errors.New("tls: failed to verify certificate")})
	if body := w.Body.String(); w.Code != http.StatusBadGateway || strings.Contains(body, "10.0.0.7") || strings.Contains(body, "/v1/messages") {
		t.Errorf("response = %d %s, want a 502 that does not disclose the upstream address", w.Code, body)
	}
}
