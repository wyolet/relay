package apply

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
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
