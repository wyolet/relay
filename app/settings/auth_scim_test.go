package settings

import (
	"testing"

	"github.com/wyolet/relay/app/settings/settingstest"
)

func TestAuthSCIMIsOffByDefaultAndAdminOnly(t *testing.T) {
	if c := AuthSCIMFrom(nil); c.Enabled || c.DeleteDisables {
		t.Fatalf("nil reader = %+v, want disabled", c)
	}
	sec, ok := Lookup(AuthSCIMSection)
	if !ok {
		t.Fatal("auth:scim not registered")
	}
	if d := sec.Defaults().(*AuthSCIM); d.Enabled {
		t.Fatal("auth:scim is on by default")
	}
	if c := AuthSCIMFrom(nil); c.EffectiveLinkClaim() != "sub" {
		t.Fatalf("default link claim = %q, want sub", c.EffectiveLinkClaim())
	}
	v, err := sec.Decode([]byte(`{"enabled":true,"deleteDisables":true,"linkClaim":"oid"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c := AuthSCIMFrom(settingstest.Sections(map[string]any{AuthSCIMSection: v})); !c.Enabled || !c.DeleteDisables || c.EffectiveLinkClaim() != "oid" {
		t.Fatalf("decoded %+v", c)
	}
	if !AdminOnly(AuthSCIMSection) {
		t.Fatal("auth:scim is writable by a non-admin")
	}
}

// Only the provider's stable user ids link a sign-in; a claim the user controls, such as an email, would let them claim another account.
func TestAuthSCIMLinkClaimIsSubOrOid(t *testing.T) {
	sec, _ := Lookup(AuthSCIMSection)
	for _, claim := range []string{"email", "preferred_username", "name", "SUB", "oid "} {
		if _, err := sec.Decode([]byte(`{"enabled":true,"linkClaim":"` + claim + `"}`)); err == nil {
			t.Errorf("linkClaim %q was accepted", claim)
		}
	}
	for _, claim := range []string{"", "sub", "oid"} {
		if _, err := sec.Decode([]byte(`{"enabled":true,"linkClaim":"` + claim + `"}`)); err != nil {
			t.Errorf("linkClaim %q: %v", claim, err)
		}
	}
}
