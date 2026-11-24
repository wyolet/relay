package key

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/meta"
)

const (
	validHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	userID    = "01a19bc3-0000-7000-8000-000000000001"
	otherID   = "01a19bc3-0000-7000-8000-000000000002"
)

// fix builds a serviceaccount-principal key in a project, the common shape.
func fix() *Key {
	return &Key{
		Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerProject, ID: otherID}},
		Spec: Spec{Principal: Principal{Kind: PrincipalServiceAccount, ID: otherID}, KeyHash: validHash, Prefix: "rk_test"},
	}
}

func userKey(k *Key) {
	k.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: userID}
	k.Spec.Principal = Principal{Kind: PrincipalUser, ID: userID}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(k *Key)
		want string
	}{
		{"serviceaccount principal with project owner", func(*Key) {}, ""},
		{"serviceaccount principal with team owner", func(k *Key) { k.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: otherID} }, ""},
		{"serviceaccount principal owned by the organization", func(k *Key) { k.Meta.Owner = meta.Global }, `key "k": serviceaccount-principal key must be owned by the service account's project, team or user, got "organization"`},
		{"user principal owned by that user", userKey, ""},
		{"user principal with the owner id left to be stamped", func(k *Key) { userKey(k); k.Meta.Owner.ID = "" }, ""},
		{"account id", func(k *Key) { k.Spec.AccountID = otherID }, ""},
		{"missing principal", func(k *Key) { k.Spec.Principal = Principal{} }, "Key: 'Key.Spec.Principal' Error:Field validation for 'Principal' failed on the 'required' tag"},
		{"principal kind not in the closed set", func(k *Key) { k.Spec.Principal.Kind = "robot" }, "Key: 'Key.Spec.Principal.Kind' Error:Field validation for 'Kind' failed on the 'oneof' tag"},
		{"principal id not an id", func(k *Key) { k.Spec.Principal.ID = "ci" }, "Key: 'Key.Spec.Principal.ID' Error:Field validation for 'ID' failed on the 'uuid' tag"},
		{"account id not an id", func(k *Key) { k.Spec.AccountID = "personal" }, "Key: 'Key.Spec.AccountID' Error:Field validation for 'AccountID' failed on the 'uuid' tag"},
		{"user principal owned by a different user", func(k *Key) { userKey(k); k.Meta.Owner.ID = otherID }, `key "k": owner.id "01a19bc3-0000-7000-8000-000000000002" does not match principal.id "01a19bc3-0000-7000-8000-000000000001"`},
		{"user principal owned by a project", func(k *Key) { k.Spec.Principal = Principal{Kind: PrincipalUser, ID: userID} }, `key "k": user-principal key must be owned by a user, got "project"`},
		{"serviceaccount principal owned by its user", func(k *Key) { k.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: userID} }, ""},
		{"serviceaccount principal owned by a host", func(k *Key) { k.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: otherID} }, `key "k": serviceaccount-principal key must be owned by the service account's project, team or user, got "host"`},
		{"missing keyhash", func(k *Key) { k.Spec.KeyHash = "" }, "Key: 'Key.Spec.KeyHash' Error:Field validation for 'KeyHash' failed on the 'required' tag"},
		{"short keyhash", func(k *Key) { k.Spec.KeyHash = "deadbeef" }, "Key: 'Key.Spec.KeyHash' Error:Field validation for 'KeyHash' failed on the 'len' tag"},
		{"non-hex keyhash", func(k *Key) { k.Spec.KeyHash = strings.Repeat("z", 64) }, "Key: 'Key.Spec.KeyHash' Error:Field validation for 'KeyHash' failed on the 'hexadecimal' tag"},
		{"short previousKeyHash", func(k *Key) { k.Spec.PreviousKeyHash = "deadbeef" }, "Key: 'Key.Spec.PreviousKeyHash' Error:Field validation for 'PreviousKeyHash' failed on the 'len' tag"},
		{"non-hex previousKeyHash", func(k *Key) { k.Spec.PreviousKeyHash = strings.Repeat("z", 64) }, "Key: 'Key.Spec.PreviousKeyHash' Error:Field validation for 'PreviousKeyHash' failed on the 'hexadecimal' tag"},
	} {
		k := fix()
		tc.edit(k)
		got := ""
		if err := k.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestIsActiveAndInGrace(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	off, on := false, true
	for _, tc := range []struct {
		name            string
		edit            func(*Key)
		active, inGrace bool
	}{
		{"default", func(*Key) {}, true, false},
		{"enabled", func(k *Key) { k.Spec.Enabled = &on }, true, false},
		{"disabled", func(k *Key) { k.Spec.Enabled = &off }, false, false},
		{"revoked", func(k *Key) { k.Spec.RevokedAt = &past }, false, false},
		{"revoked at a future time", func(k *Key) { k.Spec.RevokedAt = &future }, false, false},
		{"expired", func(k *Key) { k.Spec.ExpiresAt = &past }, false, false},
		{"expiring this instant", func(k *Key) { k.Spec.ExpiresAt = &now }, false, false},
		{"not yet expired", func(k *Key) { k.Spec.ExpiresAt = &future }, true, false},
		{"grace still open", func(k *Key) { k.Spec.PreviousKeyHash, k.Spec.GraceUntil = validHash, &future }, true, true},
		{"grace closing this instant", func(k *Key) { k.Spec.PreviousKeyHash, k.Spec.GraceUntil = validHash, &now }, true, false},
		{"grace closed", func(k *Key) { k.Spec.PreviousKeyHash, k.Spec.GraceUntil = validHash, &past }, true, false},
		{"grace deadline without a previous hash", func(k *Key) { k.Spec.GraceUntil = &future }, true, false},
		{"previous hash without a deadline", func(k *Key) { k.Spec.PreviousKeyHash = validHash }, true, false},
	} {
		k := fix()
		tc.edit(k)
		if active, inGrace := k.IsActive(now), k.InGrace(now); active != tc.active || inGrace != tc.inGrace {
			t.Errorf("%s: IsActive %v InGrace %v, want %v %v", tc.name, active, inGrace, tc.active, tc.inGrace)
		}
	}
}
