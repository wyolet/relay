package main

import (
	"go/ast"
	"go/parser"
	"go/token"
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
