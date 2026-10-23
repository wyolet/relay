package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"runtime/debug"
	"testing"
)

// The admission collector releases the in-flight slot, so it must be
// registered after the collectors doing post-flight I/O for the cap to
// cover that work. Registration order is the call order in main.
func TestAdmissionRegisteredAfterIOCollectors(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	callPos := map[string]token.Pos{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			if _, seen := callPos[id.Name]; !seen {
				callPos[id.Name] = call.Pos()
			}
		}
		return true
	})
	admission, ok := callPos["registerAdmission"]
	if !ok {
		t.Fatal("registerAdmission is not called from main.go")
	}
	for _, name := range []string{"buildUsageLog", "buildPayloadLog", "buildTokenCalibrator"} {
		pos, ok := callPos[name]
		if !ok {
			t.Fatalf("%s is not called from main.go", name)
		}
		if pos > admission {
			t.Errorf("%s registers its collector after registerAdmission; the in-flight cap would not cover its post-flight work", name)
		}
	}
}

func TestPayloadQueueSize(t *testing.T) {
	if got := payloadQueueSize(500); got != 500 {
		t.Fatalf("configured 500: got %d", got)
	}
	prev := debug.SetMemoryLimit(900 << 20)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	if got := payloadQueueSize(0); got != 900 {
		t.Fatalf("900 MiB memory limit: got %d, want 900", got)
	}
	debug.SetMemoryLimit(64 << 20)
	if got := payloadQueueSize(0); got != minDerivedPayloadQueue {
		t.Fatalf("64 MiB memory limit: got %d, want the floor %d", got, minDerivedPayloadQueue)
	}
	debug.SetMemoryLimit(math.MaxInt64)
	if got := payloadQueueSize(0); got != noMemLimitPayloadQueue {
		t.Fatalf("no memory limit: got %d, want %d", got, noMemLimitPayloadQueue)
	}
}
