package settings

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/settings/settingstest"
)

// The owner tier decides: a system row is an admin's (unless it is a Team,
// Group or Role, which are system-owned only because nobody owns them
// personally), an organization row is an admin's, a tenant's row is the tenant's whatever the section says, and
// a catalog-managed row follows its governance section, which binds an admin too.
func TestGoverns(t *testing.T) {
	const (
		allowed     = ""
		systemOwned = "system"
		orgOwned    = "organization"
		sectionOff  = "section"
	)
	locked := map[string]any{SectionGovernancePolicy: &Governance{}}
	open := map[string]any{SectionGovernancePolicy: &Governance{AllowEdit: true, AllowDelete: true}}
	wrongType := map[string]any{SectionGovernancePolicy: "not a governance section"}

	for _, tc := range []struct {
		name      string
		op        Op
		kind      string
		ownerKind string
		admin     bool
		sections  map[string]any
		want      string
	}{
		{"system host: edit", OpEdit, "host", "system", false, nil, systemOwned},
		{"system host: delete", OpDelete, "host", "system", false, nil, systemOwned},
		{"system policy: delete", OpDelete, "policy", "system", false, open, systemOwned},
		{"system rate limit: edit", OpEdit, "rate-limit", "system", false, nil, systemOwned},
		{"system host: admin edits", OpEdit, "host", "system", true, nil, allowed},
		{"system policy: admin deletes under a locked section", OpDelete, "policy", "system", true, locked, allowed},
		{"system team: edit", OpEdit, "team", "system", false, nil, allowed},
		{"system team: delete", OpDelete, "team", "system", false, nil, allowed},
		{"system group: delete", OpDelete, "group", "system", false, nil, allowed},
		{"system role: delete", OpDelete, "role", "system", false, nil, allowed},

		{"organization policy: edit", OpEdit, "policy", "organization", false, open, orgOwned},
		{"organization budget: delete", OpDelete, "budget", "organization", false, nil, orgOwned},
		// The system tier's Team, Group and Role exception does not carry over.
		{"organization team: edit", OpEdit, "team", "organization", false, nil, orgOwned},
		{"organization policy: admin edits under a locked section", OpEdit, "policy", "organization", true, locked, allowed},
		{"organization pool: admin deletes", OpDelete, "pool", "organization", true, nil, allowed},

		{"user policy: delete", OpDelete, "policy", "user", false, locked, allowed},
		{"user key: edit", OpEdit, "key", "user", false, nil, allowed},
		{"team project: delete", OpDelete, "project", "team", false, locked, allowed},
		{"project policy: edit under a locked section", OpEdit, "policy", "project", false, locked, allowed},
		{"project policy: delete under a locked section", OpDelete, "policy", "project", false, locked, allowed},

		{"provider model: edit by default", OpEdit, "model", "provider", false, nil, allowed},
		{"provider model: delete by default", OpDelete, "model", "provider", false, nil, sectionOff},
		{"host policy: delete once the section allows it", OpDelete, "policy", "host", false, open, allowed},
		{"host policy: edit under a locked section", OpEdit, "policy", "host", false, locked, sectionOff},
		{"host policy: admin edit under a locked section", OpEdit, "policy", "host", true, locked, sectionOff},
		{"host policy: a section of another type is the default", OpDelete, "policy", "host", false, wrongType, sectionOff},
		{"host policy: another kind's section does not apply", OpDelete, "host", "host", false, open, sectionOff},
		{"unknown owner kind: the catalog default", OpDelete, "policy", "", false, nil, sectionOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want error
			switch tc.want {
			case systemOwned:
				want = &MutationError{Op: tc.op, Kind: tc.kind, OwnerKind: tc.ownerKind,
					Reason: tc.kind + " is system-owned: only an admin may " + string(tc.op) + " it"}
			case orgOwned:
				want = &MutationError{Op: tc.op, Kind: tc.kind, OwnerKind: tc.ownerKind,
					Reason: tc.kind + " is owned by the organization: only an admin may " + string(tc.op) + " it"}
			case sectionOff:
				want = &MutationError{Op: tc.op, Kind: tc.kind, OwnerKind: tc.ownerKind,
					Reason: string(tc.op) + " of " + tc.kind + " is disabled on this relay; enable it via the governance:" + tc.kind + " settings section"}
			}
			got := Governs(settingstest.Sections(tc.sections), tc.op, tc.kind, tc.ownerKind, tc.admin)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("Governs (-want +got):\n%s", diff)
			}
			if got != nil && got.Error() != want.(*MutationError).Reason {
				t.Fatalf("message = %q", got.Error())
			}
		})
	}
}
