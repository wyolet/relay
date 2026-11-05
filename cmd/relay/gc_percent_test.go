package main

import "testing"

func TestGCPercent(t *testing.T) {
	for _, tc := range []struct {
		value     string
		set       bool
		want      int
		wantApply bool
	}{
		{"", false, defaultGCPercent, true},
		{"100", true, 100, true},
		{"off", true, -1, true},
		{"junk", true, 0, false},
	} {
		got, apply := gcPercent(tc.value, tc.set)
		if got != tc.want || apply != tc.wantApply {
			t.Errorf("gcPercent(%q, %v) = %d, %v; want %d, %v", tc.value, tc.set, got, apply, tc.want, tc.wantApply)
		}
	}
}
