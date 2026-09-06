package settings

import "testing"

// A Team, Group or Role is system-owned because nobody owns it personally,
// not because the relay ships it: those follow the tenant tier and stay
// mutable through CRUD, while the rows the router depends on do not. A
// project-owned row is the tenant's whatever its governance section says.
func TestGovernsOwnerTiers(t *testing.T) {
	locked := fakeReader{SectionGovernancePolicy: &Governance{AllowEdit: false, AllowDelete: false}}

	for _, tc := range []struct {
		name      string
		op        Op
		kind      string
		ownerKind string
		reader    fakeReader
		wantErr   bool
	}{
		{name: "system-owned team edits", op: OpEdit, kind: "team", ownerKind: "system"},
		{name: "system-owned team deletes", op: OpDelete, kind: "team", ownerKind: "system"},
		{name: "system-owned group deletes", op: OpDelete, kind: "group", ownerKind: "system"},
		{name: "system-owned role deletes", op: OpDelete, kind: "role", ownerKind: "system"},
		{name: "system-owned policy is the relay's own row", op: OpDelete, kind: "policy",
			ownerKind: "system", wantErr: true},
		{name: "system-owned rate limit is the relay's own row", op: OpEdit, kind: "rate-limit",
			ownerKind: "system", wantErr: true},

		// M0-11: a project's own rows ignore the catalog governance section.
		{name: "project deletes its policy", op: OpDelete, kind: "policy", ownerKind: "project", reader: locked},
		{name: "project edits its policy", op: OpEdit, kind: "policy", ownerKind: "project", reader: locked},
		{name: "project deletes itself", op: OpDelete, kind: "project", ownerKind: "project", reader: locked},
		{name: "team deletes its project", op: OpDelete, kind: "project", ownerKind: "team", reader: locked},

		// An unknown owner kind falls through to the catalog tier's safe default.
		{name: "unknown owner edits", op: OpEdit, kind: "policy", ownerKind: "provider"},
		{name: "unknown owner deletes", op: OpDelete, kind: "policy", ownerKind: "provider", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.reader
			if r == nil {
				r = fakeReader{}
			}
			err := Governs(r, tc.op, tc.kind, tc.ownerKind)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Governs(%s, %s, %s) = %v, wantErr %v", tc.op, tc.kind, tc.ownerKind, err, tc.wantErr)
			}
		})
	}
}
