package apply

import (
	"reflect"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

func TestParseSelector(t *testing.T) {
	sel, err := parseSelector("env=prod, team=platform")
	if err != nil {
		t.Fatalf("parseSelector: %v", err)
	}
	if !reflect.DeepEqual(map[string]string(sel), map[string]string{"env": "prod", "team": "platform"}) {
		t.Fatalf("parsed %v", sel)
	}
	if !sel.matches(map[string]string{"env": "prod", "team": "platform", "extra": "x"}) {
		t.Fatal("superset of the selector must match")
	}
	if sel.matches(map[string]string{"env": "prod"}) {
		t.Fatal("missing term must not match")
	}
	if labelSelector(nil).matches(map[string]string{"env": "prod"}) {
		t.Fatal("empty selector must match nothing")
	}
	if _, err := parseSelector("env"); err == nil {
		t.Fatal("malformed term must be rejected")
	}
}

func TestPrunableOwners(t *testing.T) {
	for kind, want := range map[meta.OwnerKind]bool{
		meta.OwnerUser: true, meta.OwnerTeam: true, meta.OwnerProject: true,
		meta.OwnerSystem: false, meta.OwnerProvider: false, meta.OwnerHost: false,
	} {
		if got := prunable("Policy", "p", meta.Owner{Kind: kind, ID: "owner-1"}); got != want {
			t.Fatalf("prunable(Policy, %s) = %v, want %v", kind, got, want)
		}
	}
	// A global role binding is system-owned because its owner mirrors its
	// scope; it is still a declared row and prunes under the selector.
	if !prunable("RoleBinding", "everyone-viewer", meta.Owner{Kind: meta.OwnerSystem}) {
		t.Fatal("a global role binding must be prunable")
	}
	// A built-in Role is genuinely the relay's own row and stays out.
	if prunable("Role", "admin", meta.Owner{Kind: meta.OwnerSystem}) {
		t.Fatal("a built-in role must never be pruned")
	}
}

// A Team, Group or Role is system-owned by rule (they name a scope or a
// grant), so provenance cannot decide whether apply may prune them.
func TestTenancyKindsArePrunableWhileSystemOwned(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	for _, kind := range []string{"Team", "Group", "Role", "RoleBinding"} {
		if !prunable(kind, "platform", system) {
			t.Errorf("prunable(%s, system) = false, want true", kind)
		}
	}
	if prunable("Provider", "openai", system) {
		t.Error("a system-owned catalog row must not be prunable")
	}
}
