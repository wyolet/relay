//go:build !norules

package pipeline_test

import (
	"go/build"
	"testing"
)

func TestNoNetHTTPImport(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if imp == "net/http" {
			t.Errorf("internal/pipeline must not import net/http (found in package imports)")
		}
	}
}
