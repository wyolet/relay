package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

// wantErr checks err against want: "" means no error, a trailing "…" matches by prefix (stdlib text follows), anything else must match exactly.
func wantErr(t *testing.T, err error, want string) {
	t.Helper()
	got := ""
	if err != nil {
		got = err.Error()
	}
	if prefix, ok := strings.CutSuffix(want, "…"); ok {
		if !strings.HasPrefix(got, prefix) {
			t.Fatalf("error = %q, want prefix %q", got, prefix)
		}
		return
	}
	if got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// wantJSON marshals v and requires the exact wire bytes.
func wantJSON(t *testing.T, v any, want string) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("marshal:\n got %s\nwant %s", got, want)
	}
}
