// catalog-embed reads the public catalog YAML tree and writes sdk/catalog/catalog.json.
//
// Usage:
//
//	catalog-embed [-o path] [dir]
//
// dir defaults to $RELAY_CATALOG_DIR or ../relay-catalog/data.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wyolet/relay/app/catalogembed"
	"github.com/wyolet/relay/app/manifest"
)

func main() {
	var out string
	flag.StringVar(&out, "o", filepath.Join("sdk", "catalog", "catalog.json"), "output path")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: catalog-embed [-o path] [dir]")
		flag.PrintDefaults()
	}
	flag.Parse()

	dir := flag.Arg(0)
	if dir == "" {
		dir = os.Getenv("RELAY_CATALOG_DIR")
	}
	if dir == "" {
		dir = "../relay-catalog/data"
	}

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", dir, err)
		os.Exit(2)
	}

	cat, err := catalogembed.Compose(docs, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "compose: %v\n", err)
		os.Exit(2)
	}
	if err := catalogembed.ValidateAdapters(cat); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	data, err := catalogembed.MarshalJSON(cat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(2)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", out, err)
		os.Exit(2)
	}
}
