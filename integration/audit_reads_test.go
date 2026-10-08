//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"
)

// An export and a full snapshot dump hand out the deployment's whole
// configuration, so each is recorded even though it is a read; the counts
// view is not.
func TestIntegration_BulkReadsAreAudited(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	for _, path := range []string{"/api/debug/snapshot?detail=counts", "/api/export", "/api/debug/snapshot?detail=full"} {
		if code, raw := s.adminDo(http.MethodGet, path, ""); code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, code, raw)
		}
	}
	s.waitForAuditRows(2)
	time.Sleep(1500 * time.Millisecond) // one more emitter tick: a stray counts row would land now
	evs := s.auditList("?limit=1000").Events
	got := map[string]string{}
	for _, ev := range evs {
		got[ev.Request.Path+"?"+ev.Action] = ev.Outcome.Status
	}
	if len(evs) != 2 || got["/api/export?system.export"] == "" || got["/api/debug/snapshot?debug.snapshot"] == "" {
		t.Fatalf("audit rows = %v, want one system.export and one full debug.snapshot", got)
	}
}
