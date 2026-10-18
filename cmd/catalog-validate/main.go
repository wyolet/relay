// Command catalog-validate checks a directory of catalog YAML manifests. It
// runs the graph linter (catalogvalidate.ValidateGraph) and plans the tree
// the way a first boot seed does (apply.CheckDocs). With --curation it also
// runs a published catalog's curation checks (app/catalogvalidate/curation):
// the named rules, every Model's spec.family and spec.productLine against
// families.yaml, and featured.yaml against the featured labels.
//
// Usage:
//
//	catalog-validate [--strict] [--curation [--skip <name>...] [--families <path>] [--featured <path>]] <dir>
//	catalog-validate --list
//	catalog-validate --write-featured [--featured <path>] <dir>
//
// families.yaml and featured.yaml default to the catalog repo root, one level
// above dir. A missing featured.yaml skips the featured check.
//
// Exit codes:
//
//	0  no errors (warnings may be present unless --strict)
//	1  at least one error
//	2  internal failure (couldn't load, etc.)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/catalogvalidate/curation"
	"github.com/wyolet/relay/app/families"
	"github.com/wyolet/relay/app/manifest"
)

type stringSlice []string

func (s *stringSlice) String() string     { return fmt.Sprintf("%v", *s) }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var strict, withCuration, list, writeFeatured bool
	var familiesPath, featuredPath string
	var skip stringSlice
	flag.BoolVar(&strict, "strict", false, "promote warnings to errors before exit")
	flag.BoolVar(&withCuration, "curation", false, "also run the curation rules and the families.yaml / featured.yaml checks")
	flag.Var(&skip, "skip", "with --curation, suppress a named curation rule (repeatable)")
	flag.BoolVar(&list, "list", false, "print the curation rule registry and exit")
	flag.BoolVar(&writeFeatured, "write-featured", false, "regenerate featured.yaml from Model featured labels and exit")
	flag.StringVar(&familiesPath, "families", "", "families.yaml path (default <dir>/../families.yaml)")
	flag.StringVar(&featuredPath, "featured", "", "featured.yaml path (default <dir>/../featured.yaml)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: catalog-validate [--strict] [--curation [--skip <name>...] [--families <path>] [--featured <path>]] <dir>")
		fmt.Fprintln(os.Stderr, "       catalog-validate --list")
		fmt.Fprintln(os.Stderr, "       catalog-validate --write-featured [--featured <path>] <dir>")
		flag.PrintDefaults()
	}
	flag.Parse()

	if list {
		printRules()
		return
	}
	if flag.NArg() != 1 || (!withCuration && (len(skip) > 0 || familiesPath != "")) {
		flag.Usage()
		os.Exit(2)
	}
	dir := flag.Arg(0)
	if familiesPath == "" {
		familiesPath = filepath.Join(dir, "..", "families.yaml")
	}
	if featuredPath == "" {
		featuredPath = filepath.Join(dir, "..", "featured.yaml")
	}

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", dir, err)
		os.Exit(2)
	}

	if writeFeatured {
		if err := curation.WriteFeatured(featuredPath, curation.GenerateFeatured(docs)); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", featuredPath, err)
			os.Exit(2)
		}
		fmt.Println("wrote", featuredPath)
		return
	}

	issues := catalogvalidate.ValidateGraph(docs)
	if err := apply.CheckDocs(context.Background(), seededDocs(docs)); err != nil {
		issues = append(issues, catalogvalidate.Issue{
			Severity: catalogvalidate.SeverityError, Kind: catalogvalidate.KindInvariant, Message: err.Error(),
		})
	}
	if withCuration {
		curationIssues, err := checkCuration(docs, skip, familiesPath, featuredPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		issues = append(issues, curationIssues...)
	}
	if strict {
		issues = catalogvalidate.Promote(issues)
	}

	fmt.Print(catalogvalidate.Format(issues))

	if catalogvalidate.HasErrors(issues) {
		os.Exit(1)
	}
}

func checkCuration(docs []manifest.Document, skip []string, familiesPath, featuredPath string) ([]catalogvalidate.Issue, error) {
	skipSet := make(map[string]bool, len(skip))
	for _, n := range skip {
		skipSet[n] = true
	}
	issues := catalogvalidate.RunRules(curation.Rules, docs, skipSet)

	fams, err := families.Load(familiesPath)
	if err != nil {
		return nil, err
	}
	lines := fams.LineNames()
	issues = append(issues, curation.CheckFamilies(docs, lines)...)
	issues = append(issues, curation.CheckProductLines(docs, lines)...)

	want, ok, err := curation.LoadFeatured(featuredPath)
	if err != nil {
		return nil, err
	}
	if ok {
		issues = append(issues, curation.CheckFeatured(docs, want)...)
	}
	return issues, nil
}

func printRules() {
	for _, r := range catalogvalidate.ListRules(curation.Rules) {
		fmt.Printf("%-40s %-8s %s\n", r.Name, r.Severity, r.Description)
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
