// catalog-validate runs the schema-generic graph linter
// (catalogvalidate.ValidateGraph) against a directory of YAML manifests
// and prints findings. Catalog repos with their own curation conventions
// don't use this binary directly — they import app/catalogvalidate as a
// library and compose their own []Rule slice via RunRules. See
// docs/catalog-validation.md.
//
// Usage:
//
//	catalog-validate [--strict] <dir>
//
// Flags:
//
//	--strict   promote warnings to errors before exit
//
// Exit codes:
//
//	0  no errors (warnings may be present unless --strict)
//	1  at least one error
//	2  internal failure (couldn't load, etc.)
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
)

func main() {
	var strict bool
	flag.BoolVar(&strict, "strict", false, "promote warnings to errors before exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: catalog-validate [--strict] <dir>")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	dir := flag.Arg(0)

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", dir, err)
		os.Exit(2)
	}

	issues := catalogvalidate.ValidateGraph(docs)
	if strict {
		issues = catalogvalidate.Promote(issues)
	}

	fmt.Print(catalogvalidate.Format(issues))

	if catalogvalidate.HasErrors(issues) {
		os.Exit(1)
	}
}
