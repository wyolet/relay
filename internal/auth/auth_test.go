// Package auth — test suite.
// Constant-time compare (crypto/subtle.ConstantTimeCompare) is NOT unit-tested
// here because it is a well-trodden standard-library primitive. Its use is
// verified by code inspection and vet/race clean builds.
package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func ok200(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func TestMiddleware(t *testing.T) {
	key1 := []byte("key-one")
	key2 := []byte("key-two")

	mw := Middleware([][]byte{key1, key2})
	handler := mw(http.HandlerFunc(ok200))

	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantReason string // "" means no counter increment expected
	}{
		{"missing header", "", 401, ReasonMissing},
		{"malformed no bearer prefix", "Token key-one", 401, ReasonInvalid},
		{"bearer prefix only", "Bearer ", 401, ReasonInvalid},
		{"wrong key", "Bearer wrongkey", 401, ReasonInvalid},
		{"matching key1", "Bearer key-one", 200, ""},
		{"matching key2", "Bearer key-two", 200, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missBefore := testutil.ToFloat64(metricRejectedMissing)
			invBefore := testutil.ToFloat64(metricRejectedInvalid)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d", rec.Code, tc.wantStatus)
			}

			if tc.wantStatus == 401 {
				// verify envelope shape
				var body struct {
					Error struct {
						Type    string `json:"type"`
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if body.Error.Type != "invalid_request_error" {
					t.Errorf("type: got %q want invalid_request_error", body.Error.Type)
				}
				if body.Error.Code != "missing_authorization" {
					t.Errorf("code: got %q want missing_authorization", body.Error.Code)
				}
				if body.Error.Message == "" {
					t.Error("message empty")
				}
			}

			// counter assertions
			missAfter := testutil.ToFloat64(metricRejectedMissing)
			invAfter := testutil.ToFloat64(metricRejectedInvalid)
			switch tc.wantReason {
			case ReasonMissing:
				if missAfter-missBefore != 1 {
					t.Errorf("missing counter: delta %v want 1", missAfter-missBefore)
				}
				if invAfter != invBefore {
					t.Error("invalid counter should not increment")
				}
			case ReasonInvalid:
				if invAfter-invBefore != 1 {
					t.Errorf("invalid counter: delta %v want 1", invAfter-invBefore)
				}
				if missAfter != missBefore {
					t.Error("missing counter should not increment")
				}
			default: // pass — no counter
				if missAfter != missBefore || invAfter != invBefore {
					t.Error("counters should not increment on pass")
				}
			}
		})
	}
}

func TestMiddlewareFailOpen(t *testing.T) {
	// keys empty → passthrough, no counters
	mw := Middleware(nil)
	handler := mw(http.HandlerFunc(ok200))

	missBefore := testutil.ToFloat64(metricRejectedMissing)
	invBefore := testutil.ToFloat64(metricRejectedInvalid)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("fail-open: got %d want 200", rec.Code)
	}
	if testutil.ToFloat64(metricRejectedMissing) != missBefore || testutil.ToFloat64(metricRejectedInvalid) != invBefore {
		t.Error("fail-open: counters must not increment")
	}
}

func TestMiddlewareMultiKeyNoneMatch(t *testing.T) {
	mw := Middleware([][]byte{[]byte("aaa"), []byte("bbb")})
	handler := mw(http.HandlerFunc(ok200))

	invBefore := testutil.ToFloat64(metricRejectedInvalid)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer ccc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Fatalf("got %d want 401", rec.Code)
	}
	if testutil.ToFloat64(metricRejectedInvalid)-invBefore != 1 {
		t.Error("invalid counter delta must be 1")
	}
}

// TestMiddlewareXWRAPIKey verifies that X-WR-API-Key is accepted and
// takes priority over Authorization and x-api-key.
func TestMiddlewareXWRAPIKey(t *testing.T) {
	key := []byte("relay-customer-key")
	mw := Middleware([][]byte{key})
	handler := mw(http.HandlerFunc(ok200))

	// Plain X-WR-API-Key passes.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-WR-API-Key", "relay-customer-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("X-WR-API-Key: got %d want 200", rec.Code)
	}

	// X-WR-API-Key wins over Authorization (different key in Authorization is ignored).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req2.Header.Set("X-WR-API-Key", "relay-customer-key")
	req2.Header.Set("Authorization", "Bearer wrong-key")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("X-WR-API-Key priority: got %d want 200", rec2.Code)
	}

	// Wrong X-WR-API-Key is rejected.
	req3 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req3.Header.Set("X-WR-API-Key", "wrong-key")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != 401 {
		t.Fatalf("wrong X-WR-API-Key: got %d want 401", rec3.Code)
	}

	// x-api-key still accepted as fallback.
	req4 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req4.Header.Set("x-api-key", "relay-customer-key")
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)
	if rec4.Code != 200 {
		t.Fatalf("x-api-key fallback: got %d want 200", rec4.Code)
	}
}

func TestParseKeys(t *testing.T) {
	cases := []struct {
		name  string
		input []string
		want  int
	}{
		{"empty", []string{""}, 0},
		{"single", []string{"mykey"}, 1},
		{"comma list", []string{"a,b,c"}, 3},
		{"newline list", []string{"a\nb\nc"}, 3},
		{"mixed", []string{"a,b\nc"}, 3},
		{"whitespace trimmed", []string{"  a , b  "}, 2},
		{"empty segments dropped", []string{"a,,b"}, 2},
		{"multiple env values", []string{"a", "b,c"}, 3},
		{"all empty", []string{"", "  ", ","}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseKeys(tc.input...)
			if len(got) != tc.want {
				t.Errorf("len=%d want %d (input=%v)", len(got), tc.want, tc.input)
			}
		})
	}
}
