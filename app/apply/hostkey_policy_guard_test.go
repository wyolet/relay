package apply

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

// A key whose tier policy belongs to another host is dropped from the
// snapshot, so apply refuses it with the message the API's guard uses.
func TestApplyRejectsAHostKeyOnAForeignPolicy(t *testing.T) {
	pols := map[string]*policy.Policy{
		"p-1": {Meta: meta.Metadata{ID: "p-1", Name: "openai-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}}},
		"p-2": {Meta: meta.Metadata{ID: "p-2", Name: "team-pol", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "proj-1"}}},
	}
	check := checkHostKeyPolicy(pols)
	ok := &hostkey.HostKey{Meta: meta.Metadata{Name: "k"}, Spec: hostkey.Spec{HostID: "h-1", PolicyID: "p-1"}}
	if err := check(context.Background(), ok); err != nil {
		t.Fatalf("host-owned policy of the key's own host: %v", err)
	}
	foreign := &hostkey.HostKey{Meta: meta.Metadata{Name: "k"}, Spec: hostkey.Spec{HostID: "h-1", PolicyID: "p-2"}}
	err := check(context.Background(), foreign)
	if err == nil {
		t.Fatal("a project-owned policy was accepted for a host key")
	}
	if got := err.Error(); got != `policy "team-pol" is not host-owned by host "h-1" (owner=project/proj-1)` {
		t.Errorf("message = %q", got)
	}
	// Another host's tier policy is refused too.
	otherHost := &hostkey.HostKey{Meta: meta.Metadata{Name: "k"}, Spec: hostkey.Spec{HostID: "h-2", PolicyID: "p-1"}}
	if err := check(context.Background(), otherHost); err == nil {
		t.Error("a policy owned by a different host was accepted")
	}
	// A policy this run cannot resolve is left to the snapshot's own drop.
	unknown := &hostkey.HostKey{Meta: meta.Metadata{Name: "k"}, Spec: hostkey.Spec{HostID: "h-1", PolicyID: "p-9"}}
	if err := check(context.Background(), unknown); err != nil {
		t.Errorf("unresolvable policy = %v, want it left alone", err)
	}
}
