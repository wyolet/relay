package hostkey

import "testing"

// Owners see the first characters of a long secret; everyone else only a known format prefix and the last 4. Neither shows anything of an unresolved or short secret.
func TestPrefixAndHint(t *testing.T) {
	for _, tc := range []struct{ value, prefix, hint string }{
		{"", "", ""},
		{"sk-short", "", ""},
		{"sk-ant-api03-0123456789abcdefgh", "sk-ant-a", "sk-ant-...efgh"},
		{"sk-proj-0123456789abcdefghijkl", "sk-proj-", "sk-proj-...ijkl"},
		{"AIzaSyD0123456789abcdefghijk", "AIzaSyD0", "AIza...hijk"},
		{"f3a91c0d77e24b5c9a1d2e3f4a5b6c7d", "f3a91c0d", "...6c7d"},
	} {
		k := &HostKey{Resolved: tc.value}
		if got := k.Prefix(); got != tc.prefix {
			t.Errorf("Prefix(%q) = %q, want %q", tc.value, got, tc.prefix)
		}
		if got := k.Hint(); got != tc.hint {
			t.Errorf("Hint(%q) = %q, want %q", tc.value, got, tc.hint)
		}
	}
}
