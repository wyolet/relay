package hostkey

import "testing"

// A prefix shows the first characters of a long secret only: never of an unresolved or short one.
func TestPrefix(t *testing.T) {
	for value, want := range map[string]string{
		"":                                "",
		"sk-short":                        "",
		"sk-ant-api03-0123456789abcdefgh": "sk-ant-a",
	} {
		if got := (&HostKey{Resolved: value}).Prefix(); got != want {
			t.Errorf("Prefix(%q) = %q, want %q", value, got, want)
		}
	}
}
