package relaykey

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
)

const validHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func fix(name string) *RelayKey {
	return &RelayKey{
		Meta: meta.Metadata{
			Name:  name,
			Owner: meta.Owner{Kind: meta.OwnerUser},
		},
		Spec: Spec{KeyHash: validHash, Prefix: "rk_test"},
	}
}

func TestValidate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		if err := fix("k").Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		k    *RelayKey
		want string
	}{
		{
			name: "missing keyhash",
			k:    func() *RelayKey { k := fix("k"); k.Spec.KeyHash = ""; return k }(),
			want: "KeyHash",
		},
		{
			name: "short keyhash",
			k:    func() *RelayKey { k := fix("k"); k.Spec.KeyHash = "deadbeef"; return k }(),
			want: "KeyHash",
		},
		{
			name: "non-hex keyhash",
			k: func() *RelayKey {
				k := fix("k")
				k.Spec.KeyHash = strings.Repeat("z", 64)
				return k
			}(),
			want: "KeyHash",
		},
		{
			name: "system owner rejected",
			k:    func() *RelayKey { k := fix("k"); k.Meta.Owner.Kind = meta.OwnerSystem; return k }(),
			want: "owner.kind must be user",
		},
		{
			name: "provider owner rejected",
			k: func() *RelayKey {
				k := fix("k")
				k.Meta.Owner = meta.Owner{Kind: meta.OwnerProvider, ID: meta.NewID()}
				return k
			}(),
			want: "owner.kind must be user",
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

func TestIsActive(t *testing.T) {
	now := time.Now()
	fls := false

	if !fix("a").IsActive() {
		t.Error("default key should be active")
	}
	k := fix("b")
	k.Spec.Enabled = &fls
	if k.IsActive() {
		t.Error("disabled key should not be active")
	}
	k = fix("c")
	k.Spec.RevokedAt = &now
	if k.IsActive() {
		t.Error("revoked key should not be active")
	}
}
