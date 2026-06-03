package seed

import "testing"

func TestResolverSkip(t *testing.T) {
	idx := &indexBuilder{Dirty: map[string]bool{"dirty-id": true, "clean-id": false}}

	cases := []struct {
		name       string
		id         string
		clearDirty bool
		want       bool
	}{
		{"dirty row skipped by default", "dirty-id", false, true},
		{"dirty row overwritten with --dirty", "dirty-id", true, false},
		{"clean row never skipped", "clean-id", false, false},
		{"unknown id (new row) never skipped", "new-id", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := idx.skip(c.id, c.clearDirty); got != c.want {
				t.Fatalf("skip(%q, clearDirty=%v) = %v, want %v", c.id, c.clearDirty, got, c.want)
			}
		})
	}
}
