package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
)

// licensed is a Checker that unlocks exactly the named features.
type licensed map[string]bool

func (l licensed) Has(feature string) bool { return l[feature] }

func roleDoc(name string) *manifest.RoleDTO {
	d := &manifest.RoleDTO{APIVersion: manifest.APIVersion, Kind: "Role"}
	d.Metadata.Name = name
	d.Spec.Rules = []manifest.RoleRuleDTO{{Kinds: []string{"usage"}, Verbs: []string{"read"}}}
	return d
}

// Authoring a Role is a licensed feature wherever it is authored from: a
// bundle posted to /apply is the same act as a POST /roles.
func TestApplyGatesCustomRolesOnTheLicense(t *testing.T) {
	b := &builder{rows: &Rows{}, lic: license.Community}
	err := b.checkRoleDocs([]*manifest.RoleDTO{roleDoc("auditor-lite")})
	var gate *LicenseError
	if !errors.As(err, &gate) {
		t.Fatalf("err = %v, want a LicenseError", err)
	}
	if !errors.Is(err, license.ErrRequired) {
		t.Errorf("err does not unwrap to license.ErrRequired: %v", err)
	}

	b = &builder{rows: &Rows{}, lic: licensed{license.FeatureCustomRoles: true}}
	if err := b.checkRoleDocs([]*manifest.RoleDTO{roleDoc("auditor-lite")}); err != nil {
		t.Fatalf("a licensed deployment must accept a custom role: %v", err)
	}

	// A loader below the control API (the boot seed, the CLI) supplies no
	// gate: an operator's own tree on disk is not the licensed surface.
	b = &builder{rows: &Rows{}}
	if err := b.checkRoleDocs([]*manifest.RoleDTO{roleDoc("auditor-lite")}); err != nil {
		t.Fatalf("the boot seed must not be gated: %v", err)
	}
}

// A manifest Role called `admin` would shadow the built-in every binding
// already trusts.
func TestApplyRejectsBuiltinRoleNames(t *testing.T) {
	b := &builder{rows: &Rows{}, lic: licensed{license.FeatureCustomRoles: true}}
	err := b.checkRoleDocs([]*manifest.RoleDTO{roleDoc("admin")})
	var reserved *ReservedNameError
	if !errors.As(err, &reserved) {
		t.Fatalf("err = %v, want a ReservedNameError", err)
	}
	if reserved.Name != "admin" {
		t.Errorf("reserved name reported as %q", reserved.Name)
	}
}

// Settings and identity rows have their own loaders. Dropping them silently
// makes a bundle report success while half of it went nowhere.
func TestApplyRejectsKindsItCannotWrite(t *testing.T) {
	for _, doc := range []manifest.Document{
		{Setting: &manifest.SettingDTO{Kind: "Setting"}},
		{Foreign: "User"},
	} {
		b := &builder{rows: &Rows{}, lic: license.Community}
		err := b.run(context.Background(), []manifest.Document{doc})
		var unsupported *UnsupportedKindError
		if !errors.As(err, &unsupported) {
			t.Fatalf("run(%s) = %v, want an UnsupportedKindError", doc.Kind(), err)
		}
		if unsupported.Kind != doc.Kind() {
			t.Errorf("reported kind %q, want %q", unsupported.Kind, doc.Kind())
		}
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

// A stored host key holds ciphertext (or nothing) where the manifest holds
// the plaintext it declared, so `value` can never compare equal.
func TestHostKeyValueIsNeverDrift(t *testing.T) {
	spec := hostkey.Spec{HostID: "h-1", PolicyID: "p-1"}
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
