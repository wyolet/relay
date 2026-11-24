package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/pkg/reqid"
)

// Every control response carries the relay's request id, as inference responses do, and echoes a valid client id.
func TestControlRouterSetsRequestID(t *testing.T) {
	r := controlRouter(nil)
	r.Get("/api/version", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, path := range []string{"/api/version", "/api/missing"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(reqid.HeaderInbound, "client-1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Header().Get(reqid.HeaderOutbound) == "" || w.Header().Get(reqid.HeaderInbound) != "client-1" {
			t.Errorf("GET %s: headers %v, want %s set and %s echoed", path, w.Header(), reqid.HeaderOutbound, reqid.HeaderInbound)
		}
	}
}
