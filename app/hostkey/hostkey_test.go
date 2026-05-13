package hostkey

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix(name string, kind ValueKind) *HostKey {
	k := &HostKey{
		Meta: meta.Metadata{
			Name:  name,
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()},
		},
		Spec: Spec{ValueFrom: ValueFrom{Kind: kind}},
	}
	switch kind {
	case ValueKindEnv:
		k.Spec.ValueFrom.Env = "EXAMPLE_VAR"
	case ValueKindStored:
		k.Spec.Value = "sk-test"
	}
	return k
}

func TestValidate(t *testing.T) {
	t.Run("ok env", func(t *testing.T) {
		if err := fix("k", ValueKindEnv).Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("ok stored", func(t *testing.T) {
		if err := fix("k", ValueKindStored).Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		k    *HostKey
		want string
	}{
		{
			name: "user owner rejected",
			k:    func() *HostKey { k := fix("k", ValueKindEnv); k.Meta.Owner.Kind = meta.OwnerUser; return k }(),
			want: "owner.kind must be host",
		},
		{
			name: "missing host id",
			k:    func() *HostKey { k := fix("k", ValueKindEnv); k.Meta.Owner.ID = ""; return k }(),
			want: "owner.id is required",
		},
		{
			name: "env mode missing env",
			k:    func() *HostKey { k := fix("k", ValueKindEnv); k.Spec.ValueFrom.Env = ""; return k }(),
			want: "valueFrom.env required",
		},
		{
			name: "env mode with cleartext rejected",
			k:    func() *HostKey { k := fix("k", ValueKindEnv); k.Spec.Value = "sk-x"; return k }(),
			want: "value must be empty",
		},
		{
			name: "stored mode missing value",
			k:    func() *HostKey { k := fix("k", ValueKindStored); k.Spec.Value = ""; return k }(),
			want: "value required",
		},
		{
			name: "stored mode with env rejected",
			k: func() *HostKey {
				k := fix("k", ValueKindStored)
				k.Spec.ValueFrom.Env = "X"
				return k
			}(),
			want: "valueFrom.env must be empty",
		},
		{
			name: "unknown kind",
			k: func() *HostKey {
				k := fix("k", ValueKindEnv)
				k.Spec.ValueFrom.Kind = "bogus"
				return k
			}(),
			want: "Kind",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.k.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}
