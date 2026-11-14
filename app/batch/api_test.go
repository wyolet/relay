package batch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Each request is answered before the store or queue is reached, so both are nil.
func TestRoutes_RefusalsBeforeTheStore(t *testing.T) {
	authed := func(context.Context) *Caller { return &Caller{KeyHash: "k"} }
	errBody := func(msg string) string {
		return `{"error":{"message":"` + msg + `","type":"invalid_request_error"}}` + "\n"
	}
	for _, tc := range []struct {
		name         string
		caller       CallerFunc
		method, path string
		body         string
		status       int
		msg          string
	}{
		{"submit without a resolver", nil, "POST", "/", `{}`, 401, "unauthenticated"},
		{"submit unauthenticated", func(context.Context) *Caller { return nil }, "POST", "/", `{}`, 401, "unauthenticated"},
		{"status unauthenticated", nil, "GET", "/b1", "", 401, "unauthenticated"},
		{"results unauthenticated", nil, "GET", "/b1/results", "", 401, "unauthenticated"},
		{"cancel unauthenticated", nil, "POST", "/b1/cancel", "", 401, "unauthenticated"},
		{"malformed body", authed, "POST", "/", `{"shape":`, 400, "invalid request body"},
		{"missing shape", authed, "POST", "/", `{"requests":[{}]}`, 400, "shape is required"},
		{"no requests", authed, "POST", "/", `{"shape":"openai","requests":[]}`, 400, "requests must be non-empty"},
		{"over the cap", authed, "POST", "/", `{"shape":"openai","requests":[{},{},{}]}`, 400, "batch: too many items: 3 items, at most 2 per batch"},
	} {
		w := httptest.NewRecorder()
		NewService(nil, nil, nil, tc.caller, 2).Routes().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.status || w.Body.String() != errBody(tc.msg) || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %q %s, want %d %s", tc.name, w.Code, w.Header().Get("Content-Type"), w.Body, tc.status, errBody(tc.msg))
		}
	}
	w := httptest.NewRecorder()
	NewService(nil, nil, nil, authed, 2).Routes().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/b1", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /b1 = %d, want 405", w.Code)
	}
}
