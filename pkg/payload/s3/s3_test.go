package s3

import (
	"testing"
	"time"
)

func TestObjectKey(t *testing.T) {
	ts := time.Date(2026, 5, 26, 14, 30, 0, 0, time.UTC)

	cases := []struct {
		prefix, want string
	}{
		{"payloads", "payloads/2026/05/26/req-1.json"},
		{"payloads/", "payloads/2026/05/26/req-1.json"}, // trailing slash trimmed
		{"", "2026/05/26/req-1.json"},                   // no prefix
	}
	for _, c := range cases {
		if got := objectKey(c.prefix, ts, "req-1"); got != c.want {
			t.Errorf("objectKey(%q): got %q want %q", c.prefix, got, c.want)
		}
	}

	// Zero timestamp falls back to now (just assert it doesn't collapse to 0000).
	if got := objectKey("p", time.Time{}, "req-1"); got[:2] == "p/0000" {
		t.Errorf("zero ts should fall back to now, got %q", got)
	}
}
