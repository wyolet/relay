package keypool

import "testing"

func TestCooldownCell_ExtendNeverShortens(t *testing.T) {
	c := NewCooldownCell()
	if c.Cooled(1) {
		t.Fatal("a fresh cooldown is set")
	}
	c.Extend(5000)
	c.Extend(3000)
	if got := c.Until(); got != 5000 {
		t.Fatalf("until = %d, want 5000", got)
	}
	if !c.Cooled(4999) || c.Cooled(5000) {
		t.Fatal("cooled around the end is wrong")
	}
}

func TestRotation_NextAndLastUsed(t *testing.T) {
	r := NewRotation()
	if a, b := r.Next(), r.Next(); a != 0 || b != 1 {
		t.Fatalf("next = %d, %d, want 0, 1", a, b)
	}
	r.Touch("h1", 42)
	if r.LastUsed("h1") != 42 || r.LastUsed("h2") != 0 {
		t.Fatal("last use not recorded per value")
	}
}
