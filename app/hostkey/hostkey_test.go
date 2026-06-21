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
			Owner: meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()},
		},
		Spec: Spec{
			HostID:    meta.NewID(),
			PolicyID:  meta.NewID(),
			ValueFrom: ValueFrom{Kind: kind},
		},
	}
	switch kind {
	case ValueKindEnv:
		k.Spec.ValueFrom.Env = "EXAMPLE_VAR"
	case ValueKindStored:
		k.Spec.Value = "sk-test"
	case ValueKindOAuth:
		k.Spec.Value = `{"access_token":"at","refresh_token":"rt"}`
		k.Spec.ValueFrom.Provider = "anthropic"
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
	t.Run("ok oauth", func(t *testing.T) {
		if err := fix("k", ValueKindOAuth).Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		k    *HostKey
		want string
	}{
		{
			name: "host owner rejected",
			k:    func() *HostKey { k := fix("k", ValueKindEnv); k.Meta.Owner.Kind = meta.OwnerHost; return k }(),
			want: "owner.kind must be user or system",
		},
		{
			name: "missing host id",
			k:    func() *HostKey { k := fix("k", ValueKindEnv); k.Spec.HostID = ""; return k }(),
			want: "HostID",
		},
		{
			name: "missing policy id",
			k:    func() *HostKey { k := fix("k", ValueKindEnv); k.Spec.PolicyID = ""; return k }(),
			want: "PolicyID",
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
			name: "oauth mode missing value",
			k:    func() *HostKey { k := fix("k", ValueKindOAuth); k.Spec.Value = ""; return k }(),
			want: "value (oauth token blob) required",
		},
		{
			name: "oauth mode missing provider",
			k:    func() *HostKey { k := fix("k", ValueKindOAuth); k.Spec.ValueFrom.Provider = ""; return k }(),
			want: "valueFrom.provider required",
		},
		{
			name: "oauth mode with env rejected",
			k: func() *HostKey {
				k := fix("k", ValueKindOAuth)
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
