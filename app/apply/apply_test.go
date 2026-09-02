package apply

import (
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/team"
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
		if got := prunable("Policy", "p", meta.Owner{Kind: kind}); got != want {
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

func TestChangedFieldsIgnoresServerOwnedState(t *testing.T) {
	const kind = "Team"
	enabled := true
	stored := &team.Team{
		Meta: meta.Metadata{ID: "id-1", Name: "platform", DisplayName: "Platform", Dirty: true},
		Spec: team.Spec{Enabled: &enabled},
	}
	declared := &team.Team{
		Meta: meta.Metadata{ID: "id-1", Name: "platform", DisplayName: "Platform"},
		Spec: team.Spec{Enabled: &enabled},
	}
	if got := changedFields(viewOf(kind, stored, &stored.Meta), viewOf(kind, declared, &declared.Meta)); len(got) != 0 {
		t.Fatalf("identical rows reported %v", got)
	}

	declared.Meta.DisplayName = "Platform Engineering"
	declared.Meta.Labels = map[string]string{"env": "prod"}
	disabled := false
	declared.Spec.Enabled = &disabled
	got := changedFields(viewOf(kind, stored, &stored.Meta), viewOf(kind, declared, &declared.Meta))
	want := []string{"metadata.displayName", "metadata.labels", "spec.enabled"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changedFields = %v, want %v", got, want)
	}
}

// A host key's runtime-only state (resolved value, derived policy back-refs)
// must never register as drift — only the spec the manifest authors does.
func TestChangedFieldsIgnoresDerivedHostKeyState(t *testing.T) {
	const kind = "HostKey"
	spec := hostkey.Spec{HostID: "h-1", PolicyID: "p-1"}
	stored := &hostkey.HostKey{
		Meta:     meta.Metadata{ID: "k-1", Name: "openai-main"},
		Spec:     spec,
		Resolved: "sk-live",
		Policies: []hostkey.PolicyRef{{ID: "p-9", Name: "team"}},
	}
	declared := &hostkey.HostKey{Meta: meta.Metadata{ID: "k-1", Name: "openai-main"}, Spec: spec}
	if got := changedFields(viewOf(kind, stored, &stored.Meta), viewOf(kind, declared, &declared.Meta)); len(got) != 0 {
		t.Fatalf("derived host-key state reported %v", got)
	}
}

func TestVerbOf(t *testing.T) {
	for action, want := range map[Action]Action{
		ActionCreate: ActionCreate, ActionUpdate: ActionUpdate,
		ActionUnchanged: ActionUpdate, ActionDelete: ActionDelete,
	} {
		if got := verbOf(action); got != want {
			t.Fatalf("verbOf(%s) = %s, want %s", action, got, want)
		}
	}
}
