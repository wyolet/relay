package pipeline

import "testing"

func TestCountSSEFrames(t *testing.T) {
	for in, want := range map[string]int{
		"data: a\n\ndata: b\n\n":         2,
		"data: a\r\n\r\ndata: b\r\n\r\n": 2,
		"data: a\r\rdata: b\r\r":         2,
		"data: a\n\ndata: cut":           1,
		`{"id":"x","choices":[]}`:        0,
	} {
		if got := countSSEFrames([]byte(in)); got != want {
			t.Errorf("countSSEFrames(%q) = %d, want %d", in, got, want)
		}
	}
}
