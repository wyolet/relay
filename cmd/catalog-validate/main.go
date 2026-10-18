// catalog-validate runs the schema-generic graph linter
// (catalogvalidate.ValidateGraph) against a directory of YAML manifests,
// then plans the tree the way a first boot seed does (apply.CheckDocs), and
// prints findings. Catalog repos with their own curation conventions
// don't use this binary directly — they import app/catalogvalidate as a
// library and compose their own []Rule slice via RunRules.
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

// Command catalog-validate runs the catalog graph linter and the seed's checks over a directory of YAML manifests.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/wyolet/relay/app/apply"
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
	if err := apply.CheckDocs(context.Background(), seededDocs(docs)); err != nil {
		issues = append(issues, catalogvalidate.Issue{
			Severity: catalogvalidate.SeverityError, Kind: catalogvalidate.KindInvariant, Message: err.Error(),
		})
	}
	if strict {
		issues = catalogvalidate.Promote(issues)
	}

	fmt.Print(catalogvalidate.Format(issues))

	if catalogvalidate.HasErrors(issues) {
		os.Exit(1)
	}
}

// seededDocs drops Settings, which the seed loads through its own path.
func seededDocs(docs []manifest.Document) []manifest.Document {
	out := make([]manifest.Document, 0, len(docs))
	for _, d := range docs {
		if d.Setting == nil {
			out = append(out, d)
		}
	}
	return out
}
