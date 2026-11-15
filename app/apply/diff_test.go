package apply

import (
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
)

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
	spec := hostkey.Spec{HostID: "h-1", AccountID: "0195f8a0-0000-7000-8000-000000000001"}
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

// A stored host key holds ciphertext (or nothing) where the manifest holds
// the plaintext it declared, so `value` can never compare equal.
func TestHostKeyValueIsNeverDrift(t *testing.T) {
	spec := hostkey.Spec{HostID: "h-1"}
	stored := &hostkey.HostKey{Meta: meta.Metadata{ID: "k-1", Name: "openai-main"}, Spec: spec}
	declared := &hostkey.HostKey{Meta: meta.Metadata{ID: "k-1", Name: "openai-main"}, Spec: spec}
	declared.Spec.Value = "sk-plaintext"

	got := changedFields(
		viewOf("HostKey", stored, &stored.Meta),
		viewOf("HostKey", declared, &declared.Meta),
	)
	if len(got) != 0 {
		t.Fatalf("changedFields = %v, want none", got)
	}
}
