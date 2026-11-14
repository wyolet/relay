package inference

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/adapters"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
)

// unsupportedTranslator refuses every request as one its wire shape cannot express.
type unsupportedTranslator struct{ stubV1Translator }

func (unsupportedTranslator) SerializeRequest(*pkgrelay.Request) ([]byte, error) {
	return nil, fmt.Errorf("upstream serialize_request: %w: no such mode", pkgrelay.ErrUnsupportedRequest)
}

func TestDispatch_UnsupportedRequest_Returns400(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	useSpecs(&d, adapter.NewRegistry(
		(&adapter.Spec{Name: adapters.Anthropic, DefaultPath: "/v1/messages", Auth: adapter.AuthStrategy{Header: "x-api-key"}, Translator: unsupportedTranslator{}}).Build(),
		(&adapter.Spec{Name: adapters.Canonical, Translator: pkgrelay.IdentityTranslator{}}).Build(),
	))

	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/v1/generate", nil), pr)
	w := httptest.NewRecorder()
	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.Canonical,
		Body:      []byte(`{"model":"test-model","input":"hi"}`),
		ModelName: "test-model",
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body: %s", w.Code, w.Body.String())
	}
	e := parseDispatchErr(t, w.Body.Bytes())
	if e.Error.Code != "unsupported_request" || e.Error.Type != "invalid_request_error" {
		t.Errorf("error = %+v, want invalid_request_error / unsupported_request", e.Error)
	}
}

// A serialize failure that is not the caller's stays a server error.
func TestDispatch_SerializeFailure_Returns500(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/v1/generate", nil), pr)
	w := httptest.NewRecorder()
	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.Canonical,
		Body:      []byte(`{"model":"test-model","input":"hi"}`),
		ModelName: "test-model",
	})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500; body: %s", w.Code, w.Body.String())
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "marshal_request" {
		t.Errorf("code %q, want marshal_request", e.Error.Code)
	}
}
