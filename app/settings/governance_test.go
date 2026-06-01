package settings

import "testing"

type fakeReader map[string]any

func (f fakeReader) Setting(section string) (any, bool) {
	v, ok := f[section]
	return v, ok
}

func TestGoverns(t *testing.T) {
	tests := []struct {
		name      string
		op        Op
		kind      string
		ownerKind string
		reader    fakeReader
		wantErr   bool
	}{
		{name: "system delete always denied", op: OpDelete, kind: "rate-limit", ownerKind: "system", wantErr: true},
		{name: "system edit denied via generic CRUD", op: OpEdit, kind: "host", ownerKind: "system", wantErr: true},
		{name: "user delete allowed", op: OpDelete, kind: "policy", ownerKind: "user", wantErr: false},
		{name: "user edit allowed", op: OpEdit, kind: "relay-key", ownerKind: "user", wantErr: false},

		// Catalog-managed (host/provider-owned), no section present → safe default.
		{name: "catalog edit default allowed", op: OpEdit, kind: "model", ownerKind: "provider", wantErr: false},
		{name: "catalog delete default denied", op: OpDelete, kind: "model", ownerKind: "provider", wantErr: true},

		// Section override.
		{
			name: "catalog delete enabled via section", op: OpDelete, kind: "policy", ownerKind: "host",
			reader:  fakeReader{SectionGovernancePolicy: &Governance{AllowEdit: true, AllowDelete: true}},
			wantErr: false,
		},
		{
			name: "catalog edit disabled via section", op: OpEdit, kind: "policy", ownerKind: "host",
			reader:  fakeReader{SectionGovernancePolicy: &Governance{AllowEdit: false, AllowDelete: false}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.reader
			if r == nil {
				r = fakeReader{}
			}
			err := Governs(r, tt.op, tt.kind, tt.ownerKind)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Governs(%s,%s,%s) err=%v, wantErr=%v", tt.op, tt.kind, tt.ownerKind, err, tt.wantErr)
			}
		})
	}
}
