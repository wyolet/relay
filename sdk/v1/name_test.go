package v1

import "testing"

func TestRegistry_Get(t *testing.T) {
	reg := Registry{"identity": IdentityTranslator{}, "pass": passTranslator{}}
	if got := reg.Get("pass"); got != (passTranslator{}) {
		t.Errorf("Get(pass) = %#v, want the registered translator", got)
	}
	if got := reg.Get("absent"); got != nil {
		t.Errorf("Get(absent) = %#v, want nil", got)
	}
	if got := Registry(nil).Get("identity"); got != nil {
		t.Errorf("nil registry Get = %#v, want nil", got)
	}
}
