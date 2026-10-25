package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapters"
)

const duplicateModelBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"model":"other-model"}`

func assertDuplicateModel(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", w.Code, w.Body)
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Type != "invalid_request_error" || e.Error.Code != "duplicate_model" {
		t.Fatalf("envelope %s/%s, want invalid_request_error/duplicate_model", e.Error.Type, e.Error.Code)
	}
}

// Normal mode routes on encoding/json's last model while a byte-pass may forward both, so the body is refused before routing.
func TestHandleShape_NormalMode_DuplicateModel(t *testing.T) {
	cat, prin := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(duplicateModelBody)), prin)
	w := httptest.NewRecorder()
	handleShape(d.Specs.Spec(adapters.OpenAI), d, w, r)
	assertDuplicateModel(t, w)
}

func TestHandleCountTokens_DuplicateModel(t *testing.T) {
	cat, prin := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens", strings.NewReader(duplicateModelBody)), prin)
	w := httptest.NewRecorder()
	handleCountTokens(d, w, r)
	assertDuplicateModel(t, w)
}

// Normal mode rewrites the model key before forwarding, so it keeps routing a key spelled in another case or with escapes; proxy mode refuses those (TestProxyShape_ModelKeySpelling).
func TestParseRequest_ModelKeySpelling(t *testing.T) {
	for _, body := range []string{`{"Model":"test-model","messages":[]}`, `{"` + uEscape("006d") + `odel":"test-model","messages":[]}`} {
		if req, err := parseRequest([]byte(body), vendorKeys); err != nil || req.Model != "test-model" {
			t.Fatalf("%s: (%+v, %v), want the model", body, req, err)
		}
	}
}
