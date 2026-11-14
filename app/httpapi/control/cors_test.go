package control

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// Only a listed origin is echoed, with credentials; its preflight is answered here with the allowed methods and headers, any other preflight is refused, and every response varies on Origin so a cache never replays an answer without the allow header to a cross-origin caller.
func TestCORS(t *testing.T) {
	const ui = "https://relay.wyolet.dev"
	allowed := map[string]string{"Vary": "Origin", "Access-Control-Allow-Origin": ui, "Access-Control-Allow-Credentials": "true"}
	preflight := map[string]string{"Vary": "Origin", "Access-Control-Allow-Origin": ui, "Access-Control-Allow-Credentials": "true",
		"Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, PATCH, OPTIONS",
		"Access-Control-Allow-Headers": "content-type, x-relay-admin-token, authorization", "Access-Control-Max-Age": "600"}
	bare := map[string]string{"Vary": "Origin"}

	for _, tc := range []struct {
		name, method, origin string
		status               int
		reachesNext          bool
		headers              map[string]string
	}{
		{"preflight from the UI", http.MethodOptions, ui, 204, false, preflight},
		{"preflight from elsewhere", http.MethodOptions, "https://evil.example", 403, false, bare},
		{"preflight with no origin", http.MethodOptions, "", 403, false, bare},
		{"an origin differing in scheme", http.MethodOptions, "http://relay.wyolet.dev", 403, false, bare},
		{"an origin with the UI as a prefix", http.MethodOptions, ui + ".evil.example", 403, false, bare},
		{"request from the UI", http.MethodGet, ui, 200, true, allowed},
		{"request from elsewhere reaches the handler unblessed", http.MethodGet, "https://evil.example", 200, true, bare},
		{"same-origin request", http.MethodGet, "", 200, true, bare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			h := CORS(ui, "https://other.example")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(tc.method, "/config.json", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			got := map[string]string{}
			for k := range rec.Header() {
				got[k] = rec.Header().Get(k)
			}
			if diff := cmp.Diff(tc.headers, got); rec.Code != tc.status || reached != tc.reachesNext || diff != "" {
				t.Fatalf("status %d, next reached %v, headers (-want +got):\n%s", rec.Code, reached, diff)
			}
		})
	}
}
