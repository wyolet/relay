package user

import (
	"testing"

	"github.com/wyolet/relay/auth/password"
)

func TestHasRole(t *testing.T) {
	for _, tc := range []struct {
		roles []string
		role  string
		want  bool
	}{
		{[]string{"admin"}, RoleAdmin, true},
		{[]string{"viewer", "admin"}, RoleAdmin, true},
		{[]string{"admin"}, "other", false},
		{[]string{"administrator", "Admin"}, RoleAdmin, false},
		{nil, RoleAdmin, false},
		{[]string{""}, "", true},
	} {
		if got := (&User{Roles: tc.roles}).HasRole(tc.role); got != tc.want {
			t.Errorf("%v HasRole(%q) = %v, want %v", tc.roles, tc.role, got, tc.want)
		}
	}
}

func TestVerifyPassword(t *testing.T) {
	hash, err := password.Hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		hash, cleartext string
		want            bool
	}{
		{"bcrypt match", hash, "s3cret", true},
		{"bcrypt mismatch", hash, "wrong", false},
		{"bcrypt hash sent as the password", hash, hash, false},
		{"empty password against a hash", hash, "", false},
		{"legacy plain match", "legacy-plain", "legacy-plain", true},
		{"legacy plain mismatch", "legacy-plain", "legacy-plaim", false},
		{"no hash (an OIDC-only user)", "", "anything", false},
		{"no hash and no password", "", "", false},
	} {
		if got := VerifyPassword(tc.hash, tc.cleartext); got != tc.want {
			t.Errorf("%s: VerifyPassword = %v, want %v", tc.name, got, tc.want)
		}
	}
}
