package settings

import (
	"testing"

	"github.com/wyolet/relay/app/settings/settingstest"
)

// A Team, Group or Role is system-owned because nobody owns it personally,
// not because the relay ships it: those follow the tenant tier and stay
// mutable through CRUD, while the rows the router depends on are edited and
// deleted only by an admin. A project-owned row is the tenant's whatever its
// governance section says.
func TestGovernsOwnerTiers(t *testing.T) {
	locked := map[string]any{SectionGovernancePolicy: &Governance{AllowEdit: false, AllowDelete: false}}

	for _, tc := range []struct {
		name      string
		op        Op
		kind      string
		ownerKind string
		admin     bool
		reader    map[string]any
		wantErr   bool
	}{
		// An admin may edit or delete a system row through CRUD; nobody else may.
		{name: "admin edits system policy", op: OpEdit, kind: "policy", ownerKind: "system", admin: true},
		{name: "admin edits system host", op: OpEdit, kind: "host", ownerKind: "system", admin: true},
		{name: "admin deletes system policy", op: OpDelete, kind: "policy", ownerKind: "system", admin: true},
		{name: "admin deletes system host", op: OpDelete, kind: "host", ownerKind: "system", admin: true},
		{name: "non-admin cannot edit system host", op: OpEdit, kind: "host", ownerKind: "system", wantErr: true},
		{name: "non-admin cannot delete system host", op: OpDelete, kind: "host", ownerKind: "system", wantErr: true},
		// The governance section still binds an admin on catalog-managed rows.
		{name: "admin still bound by a locked section", op: OpEdit, kind: "policy", ownerKind: "host",
			admin: true, reader: locked, wantErr: true},

		{name: "system-owned team edits", op: OpEdit, kind: "team", ownerKind: "system"},
		{name: "system-owned team deletes", op: OpDelete, kind: "team", ownerKind: "system"},
		{name: "system-owned group deletes", op: OpDelete, kind: "group", ownerKind: "system"},
		{name: "system-owned role deletes", op: OpDelete, kind: "role", ownerKind: "system"},
		{name: "system-owned policy is the relay's own row", op: OpDelete, kind: "policy",
			ownerKind: "system", wantErr: true},
		{name: "system-owned rate limit is the relay's own row", op: OpEdit, kind: "rate-limit",
			ownerKind: "system", wantErr: true},

		// A project's own rows ignore the catalog governance section.
		{name: "project deletes its policy", op: OpDelete, kind: "policy", ownerKind: "project", reader: locked},
		{name: "project edits its policy", op: OpEdit, kind: "policy", ownerKind: "project", reader: locked},
		{name: "project deletes itself", op: OpDelete, kind: "project", ownerKind: "project", reader: locked},
		{name: "team deletes its project", op: OpDelete, kind: "project", ownerKind: "team", reader: locked},

		// An unknown owner kind falls through to the catalog tier's safe default.
		{name: "unknown owner edits", op: OpEdit, kind: "policy", ownerKind: "provider"},
		{name: "unknown owner deletes", op: OpDelete, kind: "policy", ownerKind: "provider", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Governs(settingstest.Sections(tc.reader), tc.op, tc.kind, tc.ownerKind, tc.admin)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Governs(%s, %s, %s, admin=%v) = %v, wantErr %v", tc.op, tc.kind, tc.ownerKind, tc.admin, err, tc.wantErr)
			}
		})
	}
}
