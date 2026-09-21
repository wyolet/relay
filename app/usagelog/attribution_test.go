package usagelog

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// The event's attribution comes from the lifecycle Context alone — the
// buffered and streaming producers must land identical rows, and neither may
// re-resolve a name post-flight.
func TestAttributionCopiedFromContext(t *testing.T) {
	lc := lifecycle.NewContext("req-1", "pipeline", time.Now().UTC())
	lc.RelayKeyHash = "hash"
	lc.ProjectID, lc.ProjectName = "p1", "ml-search"
	lc.TeamID, lc.TeamName = "t1", "platform"
	lc.PrincipalKind, lc.PrincipalID, lc.PrincipalName = "serviceaccount", "sa1", "indexer"
	lc.CredentialKind, lc.CredentialID = "key", "k1"

	hooked, _ := NewUsageHook(nil, "").Fill(lc, &lifecycle.PostFlightEvent{Status: 200})
	streamed, _ := NewStreamUsageFactory(nil, "").NewObserver(lc).Result()

	for name, got := range map[string]*Event{"hook": hooked.(*Event), "stream": streamed.(*Event)} {
		if got.ProjectID != "p1" || got.Project != "ml-search" {
			t.Fatalf("%s project: %q / %q", name, got.ProjectID, got.Project)
		}
		if got.TeamID != "t1" || got.Team != "platform" {
			t.Fatalf("%s team: %q / %q", name, got.TeamID, got.Team)
		}
		if got.PrincipalKind != "serviceaccount" || got.PrincipalID != "sa1" || got.Principal != "indexer" {
			t.Fatalf("%s principal: %q / %q / %q", name, got.PrincipalKind, got.PrincipalID, got.Principal)
		}
		if got.CredentialKind != "key" || got.CredentialID != "k1" {
			t.Fatalf("%s credential: %q / %q", name, got.CredentialKind, got.CredentialID)
		}
		if got.RelayKeyHash != "hash" {
			t.Fatalf("%s relay_key_hash: %q", name, got.RelayKeyHash)
		}
	}
}
