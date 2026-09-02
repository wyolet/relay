package key

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
)

const validHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fix builds a serviceaccount-principal key in a project, the common shape.
func fix(name string) *Key {
	return &Key{
		Meta: meta.Metadata{
			Name:  name,
			Owner: meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()},
		},
		Spec: Spec{
			Principal: Principal{Kind: PrincipalServiceAccount, ID: meta.NewID()},
			PolicyID:  meta.NewID(),
			KeyHash:   validHash,
			Prefix:    "rk_test",
		},
	}
}

func userKey(name string) *Key {
	uid := meta.NewID()
	k := fix(name)
	k.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: uid}
	k.Spec.Principal = Principal{Kind: PrincipalUser, ID: uid}
	return k
}

func TestValidate(t *testing.T) {
	t.Run("serviceaccount principal with project owner", func(t *testing.T) {
		if err := fix("k").Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	t.Run("user principal owned by that user", func(t *testing.T) {
		if err := userKey("k").Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	t.Run("empty policyId is allowed (policy-less key)", func(t *testing.T) {
		k := fix("k")
		k.Spec.PolicyID = ""
		if err := k.Validate(); err != nil {
			t.Fatalf("policy-less should validate, got %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		k    *Key
		want string
	}{
		{
			name: "missing principal",
			k:    func() *Key { k := fix("k"); k.Spec.Principal = Principal{}; return k }(),
			want: "Principal",
		},
		{
			name: "principal kind not in the closed set",
			k:    func() *Key { k := fix("k"); k.Spec.Principal.Kind = "robot"; return k }(),
			want: "Principal",
		},
		{
			name: "user principal owned by a different user",
			k: func() *Key {
				k := userKey("k")
				k.Meta.Owner.ID = meta.NewID()
				return k
			}(),
			want: "does not match principal.id",
		},
		{
			name: "user principal owned by a project",
			k: func() *Key {
				k := userKey("k")
				k.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}
				return k
			}(),
			want: "must be owned by a user",
		},
		{
			name: "serviceaccount principal owned by a user",
			k: func() *Key {
				k := fix("k")
				k.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}
				return k
			}(),
			want: "must be owned by a project",
		},
		{
			name: "policyId not uuid",
			k:    func() *Key { k := fix("k"); k.Spec.PolicyID = "not-a-uuid"; return k }(),
			want: "PolicyID",
		},
		{
			name: "missing keyhash",
			k:    func() *Key { k := fix("k"); k.Spec.KeyHash = ""; return k }(),
			want: "KeyHash",
		},
		{
			name: "short keyhash",
			k:    func() *Key { k := fix("k"); k.Spec.KeyHash = "deadbeef"; return k }(),
			want: "KeyHash",
		},
		{
			name: "non-hex keyhash",
			k: func() *Key {
				k := fix("k")
				k.Spec.KeyHash = strings.Repeat("z", 64)
				return k
			}(),
			want: "KeyHash",
		},
		{
			name: "non-hex previousKeyHash",
			k: func() *Key {
				k := fix("k")
				k.Spec.PreviousKeyHash = strings.Repeat("z", 64)
				return k
			}(),
			want: "PreviousKeyHash",
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

func TestIsActiveAndInGrace(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	fls := false

	for _, tc := range []struct {
		name        string
		mutate      func(*Key)
		wantActive  bool
		wantInGrace bool
	}{
		{name: "default", wantActive: true},
		{name: "disabled", mutate: func(k *Key) { k.Spec.Enabled = &fls }},
		{name: "revoked", mutate: func(k *Key) { k.Spec.RevokedAt = &past }},
		{name: "expired", mutate: func(k *Key) { k.Spec.ExpiresAt = &past }},
		{name: "not yet expired", mutate: func(k *Key) { k.Spec.ExpiresAt = &future }, wantActive: true},
		{
			name: "grace still open",
			mutate: func(k *Key) {
				k.Spec.PreviousKeyHash = validHash
				k.Spec.GraceUntil = &future
			},
			wantActive:  true,
			wantInGrace: true,
		},
		{
			name: "grace closed",
			mutate: func(k *Key) {
				k.Spec.PreviousKeyHash = validHash
				k.Spec.GraceUntil = &past
			},
			wantActive: true,
		},
		{
			name:       "grace deadline without a previous hash",
			mutate:     func(k *Key) { k.Spec.GraceUntil = &future },
			wantActive: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := fix("k")
			if tc.mutate != nil {
				tc.mutate(k)
			}
			if got := k.IsActive(now); got != tc.wantActive {
				t.Errorf("IsActive = %v, want %v", got, tc.wantActive)
			}
			if got := k.InGrace(now); got != tc.wantInGrace {
				t.Errorf("InGrace = %v, want %v", got, tc.wantInGrace)
			}
		})
	}
}
