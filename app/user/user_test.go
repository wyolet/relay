package user

import "testing"

func TestVerifyPassword(t *testing.T) {
	hash, err := HashPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "s3cret") {
		t.Error("bcrypt match failed")
	}
	if VerifyPassword(hash, "wrong") {
		t.Error("bcrypt mismatch accepted")
	}
	if !VerifyPassword("legacy-plain", "legacy-plain") {
		t.Error("legacy plain match failed")
	}
	if VerifyPassword("", "anything") {
		t.Error("empty hash must never match (OIDC-only user)")
	}
	if VerifyPassword(hash, "") {
		t.Error("empty password must never match")
	}
}

func TestHasRole(t *testing.T) {
	u := &User{Roles: []string{"admin"}}
	if !u.HasRole(RoleAdmin) || u.HasRole("other") {
		t.Errorf("HasRole wrong: %v", u.Roles)
	}
	if (&User{}).HasRole(RoleAdmin) {
		t.Error("empty roles must not match")
	}
}
