//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"
)

// An apply is audited change by change — each row the bundle writes is its
// own entry under the verb it was authorized with — and a dry run, which
// writes nothing, leaves no row.
func TestIntegration_ApplyAuditsEachChange(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	if code, _, raw := s.applyBundle(bundle, "dryRun=true"); code != http.StatusOK {
		t.Fatalf("dry run: %d %s", code, raw)
	}
	if code, _, raw := s.applyBundle(bundle, ""); code != http.StatusOK {
		t.Fatalf("apply: %d %s", code, raw)
	}
	s.waitForAuditRows(4)
	time.Sleep(1500 * time.Millisecond) // one more emitter tick: a stray dry-run row would land now
	evs := s.auditList("?limit=1000").Events
	got := map[string]string{}
	for _, ev := range evs {
		got[ev.Action+" "+ev.Resource.Name] = ev.Outcome.Status
	}
	want := []string{"teams.create platform", "projects.create ml-search", "policies.create ml-search-default", "service-accounts.create search-indexer"}
	if len(evs) != len(want) {
		t.Fatalf("audit rows = %v, want exactly %v", got, want)
	}
	for _, w := range want {
		if got[w] != "allowed" {
			t.Fatalf("audit rows = %v, missing %q", got, w)
		}
	}
}
