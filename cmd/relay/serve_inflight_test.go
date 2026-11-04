package main

import (
	"math"
	"runtime/debug"
	"testing"

	"github.com/wyolet/relay/app/httpapi"
)

func TestInferenceMaxInflight(t *testing.T) {
	if got := inferenceMaxInflight(500); got != 500 {
		t.Fatalf("configured 500: got %d", got)
	}
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	for _, tc := range []struct {
		name  string
		limit int64
		want  int
	}{
		{"900 MiB memory limit stays at the floor", 900 << 20, httpapi.DefaultMaxInflight},
		{"4 GiB memory limit", 4 << 30, 6553},
		{"16 GiB memory limit", 16 << 30, 26214},
		{"no memory limit", math.MaxInt64, httpapi.DefaultMaxInflight},
	} {
		debug.SetMemoryLimit(tc.limit)
		if got := inferenceMaxInflight(0); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
