// catalog-validate runs catalogvalidate.ValidateGraph against a directory
// of YAML manifests and prints findings. Designed for invocation from the
// catalog repo's CI:
//
//	go run github.com/wyolet/relay/cmd/catalog-validate ./data
//
// Exit codes:
//
//	0  no errors (warnings may be present)
//	1  at least one error
//	2  internal failure (couldn't load, etc.)
//
// The same package (app/catalogvalidate) is importable; CI in the catalog
// repo can compose additional curation rules on top.
package main

import (
	"fmt"
	"os"

	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: catalog-validate <dir>")
		os.Exit(2)
	}
	dir := os.Args[1]
	docs, err := manifest.LoadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", dir, err)
		os.Exit(2)
	}

	issues := catalogvalidate.ValidateGraph(docs)
	fmt.Print(catalogvalidate.Format(issues))

	if catalogvalidate.HasErrors(issues) {
		os.Exit(1)
	}
}
