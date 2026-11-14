package batch

import (
	"context"
	"errors"
	"testing"
)

// Both refusals come before anything is written, so the store and queue are nil.
func TestService_SubmitRefusesBeforeWriting(t *testing.T) {
	item := []byte(`{}`)
	overDefault := "batch: too many items: 50001 items, at most 50000 per batch"
	for _, tc := range []struct {
		name     string
		maxItems int
		items    [][]byte
		wantCap  bool
		wantMsg  string
	}{
		{"no items", 2, nil, false, "batch: no items"},
		{"one over the cap", 2, [][]byte{item, item, item}, true, "batch: too many items: 3 items, at most 2 per batch"},
		{"zero cap is the default", 0, make([][]byte, DefaultMaxItems+1), true, overDefault},
		{"negative cap is the default", -1, make([][]byte, DefaultMaxItems+1), true, overDefault},
	} {
		id, err := NewService(nil, nil, nil, nil, tc.maxItems).Submit(context.Background(), &Caller{KeyHash: "k"}, "openai", tc.items)
		if id != "" || err == nil || err.Error() != tc.wantMsg || errors.Is(err, ErrTooManyItems) != tc.wantCap {
			t.Errorf("%s: Submit = %q, %v; want %q (ErrTooManyItems=%v)", tc.name, id, err, tc.wantMsg, tc.wantCap)
		}
	}
}
