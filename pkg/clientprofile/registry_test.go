package clientprofile

import "testing"

func TestRegister_RejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "Claude-Code", "claude_code", "-alpha", "alpha-", "al--pha", "alpha/beta"} {
		if err := New().Register(stub{name: name}); err == nil {
			t.Errorf("Register(%q) = nil, want error", name)
		}
	}
	if err := New().Register(nil); err == nil {
		t.Error("Register(nil) = nil, want error")
	}
}

func TestRegister_AcceptsDNSLabels(t *testing.T) {
	reg := New()
	for _, name := range []string{"alpha", "claude-code", "a1", "a-b-c"} {
		if err := reg.Register(stub{name: name}); err != nil {
			t.Errorf("Register(%q): %v", name, err)
		}
	}
	if got := len(reg.Profiles()); got != 4 {
		t.Fatalf("Profiles len = %d, want 4", got)
	}
	if reg.Profiles()[1].Name() != "claude-code" {
		t.Error("Profiles must keep registration order")
	}
	if _, ok := reg.ByName("claude-code"); !ok {
		t.Error("ByName missed a registered profile")
	}
	if _, ok := reg.ByName("nope"); ok {
		t.Error("ByName returned an unregistered profile")
	}
}

func TestRegister_RejectsDuplicates(t *testing.T) {
	reg := New()
	if err := reg.Register(stub{name: "alpha"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := reg.Register(stub{name: "alpha"}); err == nil {
		t.Fatal("duplicate register = nil, want error")
	}
}
