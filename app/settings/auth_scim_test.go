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
