package keypool

import "testing"

func TestCooldownCell_ExtendNeverShortens(t *testing.T) {
	c := NewCooldownCell()
	if c.Cooled(1, "m") {
		t.Fatal("a fresh cooldown is set")
	}
	c.Extend(5000)
	c.Extend(3000)
	if got := c.Until("m"); got != 5000 {
		t.Fatalf("until = %d, want 5000", got)
	}
	if !c.Cooled(4999, "m") || c.Cooled(5000, "m") {
		t.Fatal("cooled around the end is wrong")
	}
}

// A model cooldown holds that model only, never shortens, and a whole-account cooldown covers every model.
func TestCooldownCell_PerModel(t *testing.T) {
	c := NewCooldownCell()
	c.ExtendModel("m", 5000, 0)
	c.ExtendModel("m", 3000, 0)
	if got := c.Until("m"); got != 5000 {
		t.Fatalf("until m = %d, want 5000", got)
	}
	if !c.Cooled(4999, "m") || c.Cooled(5000, "m") {
		t.Fatal("cooled around the model's end is wrong")
	}
	if c.Cooled(1, "n") {
		t.Fatal("another model is cooled")
	}
	c.Extend(7000)
	if !c.Cooled(6000, "n") || c.Until("m") != 7000 {
		t.Fatal("the whole-account cooldown does not cover every model")
	}
}

// A write drops the entries already over, so a cell that cooled many models once does not keep them.
func TestCooldownCell_ExtendModelDropsExpired(t *testing.T) {
	c := NewCooldownCell()
	c.ExtendModel("old", 1000, 0)
	c.ExtendModel("live", 9000, 0)
	c.ExtendModel("new", 9000, 2000)
	m := *c.models.Load()
	if _, ok := m["old"]; ok || len(m) != 2 {
		t.Fatalf("models = %v, want live and new only", m)
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
