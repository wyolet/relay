package control

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
)

func TestPlanPolicyChange(t *testing.T) {
	const acct, other, polA, polB = "acct", "other", "pol-a", "pol-b"
	off := false
	held := func(policyID string, enabled *bool, accounts ...string) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: "held", Name: "held"},
			Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: accounts, Enabled: enabled},
		}
	}
	ptr := func(s string) *string { return &s }

	for _, tc := range []struct {
		name     string
		held     *policybinding.PolicyBinding
		policyID *string
		want     policyChange
		shrunkTo []string
	}{
		{name: "unbound, unbind", policyID: nil, want: policyChange{unchanged: true}},
		{name: "unbound, bind", policyID: ptr(polA), want: policyChange{create: true}},
		{name: "same policy, enabled", held: held(polA, nil, acct, other), policyID: ptr(polA), want: policyChange{unchanged: true}},
		{name: "same policy, disabled", held: held(polA, &off, acct), policyID: ptr(polA), want: policyChange{deleteHeld: true, create: true}},
		{name: "move out of a shared binding", held: held(polA, nil, other, acct), policyID: ptr(polB), want: policyChange{create: true}, shrunkTo: []string{other}},
		{name: "move out of its own binding", held: held(polA, nil, acct), policyID: ptr(polB), want: policyChange{deleteHeld: true, create: true}},
		{name: "unbind from a shared binding", held: held(polA, nil, acct, other), policyID: nil, shrunkTo: []string{other}},
		{name: "unbind from its own binding", held: held(polA, nil, acct), policyID: nil, want: policyChange{deleteHeld: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before []string
			if tc.held != nil {
				before = slices.Clone(tc.held.Spec.AccountIDs)
			}
			got := planPolicyChange(tc.held, acct, tc.policyID)
			if got.unchanged != tc.want.unchanged || got.deleteHeld != tc.want.deleteHeld || got.create != tc.want.create {
				t.Fatalf("plan = %+v, want %+v", got, tc.want)
			}
			switch {
			case tc.shrunkTo == nil && got.shrunk != nil:
				t.Fatalf("shrunk = %v, want none", got.shrunk.Spec.AccountIDs)
			case tc.shrunkTo != nil && (got.shrunk == nil || !slices.Equal(got.shrunk.Spec.AccountIDs, tc.shrunkTo)):
				t.Fatalf("shrunk = %+v, want accounts %v", got.shrunk, tc.shrunkTo)
			}
			if tc.held != nil && !slices.Equal(tc.held.Spec.AccountIDs, before) {
				t.Fatalf("the holding binding was changed in place: %v, was %v", tc.held.Spec.AccountIDs, before)
			}
		})
	}
}
