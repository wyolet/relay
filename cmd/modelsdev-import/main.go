// modelsdev-import fetches the models.dev dataset (https://models.dev/api.json)
// and writes relay-catalog YAML — Provider, Host, Model, HostBinding, and
// Pricing — for an allowlist of providers.
//
// Each models.dev provider becomes one relay Provider + Host; its models are
// owned by the provider and bound to the host. The provider's @ai-sdk npm tag
// selects the adapter; the verbatim models.dev model key is preserved as the
// binding UpstreamName (the exact wire string). Providers whose wire shape
// has no relay adapter yet (Cohere, Bedrock) are written under drafts/.
//
// Usage:
//
//	modelsdev-import -out ../relay-catalog/data
//	modelsdev-import -source-file /tmp/md.json -hosts anthropic,openai -out -
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/manifest"
)

// defaultAllow is the blessed top-tier provider set shipped out of draft.
var defaultAllow = []string{
	"anthropic", "openai", "google", // frontier
	"deepseek", "xai", "mistral", "groq", "cerebras", "perplexity", // second tier
	// first-party model-makers (mostly the China labs that dominate OR traffic).
	// minimax speaks the Anthropic wire shape; the rest are openai-compatible.
	// Canonical international keys only — the -cn/-coding-plan/-token-plan
	// shards are the same models at regional/billing endpoints.
	"moonshotai", "minimax", "alibaba", "zai", "xiaomi", "stepfun",
}

func main() {
	fs := flag.NewFlagSet("modelsdev-import", flag.ExitOnError)
	out := fs.String("out", "../relay-catalog/data", `Output catalog root. Use "-" for stdout.`)
	hosts := fs.String("hosts", strings.Join(defaultAllow, ","), "Comma-separated provider allowlist (empty = all).")
	sourceURL := fs.String("source-url", DefaultModelsDevURL, "models.dev dataset URL.")
	sourceFile := fs.String("source-file", "", "Read JSON from a local file instead of fetching.")
	fs.BoolVar(&overwriteHosts, "overwrite-hosts", false, "Overwrite existing provider.yaml/host.yaml (default: preserve hand-curated ones).")
	draft := fs.Bool("draft", true, "Route all output under drafts/ for human review before promotion.")
	additive := fs.Bool("additive", true, "Skip models whose name already exists in the target catalog (never touch curated/imported entries).")
	draftAllProviders := fs.Bool("draft-all", false, "Draft EVERY models.dev provider (not just -hosts). -hosts then only defines the supported-hosts manifest.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	providers, err := Fetch(context.Background(), *sourceURL, *sourceFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "modelsdev-import: fetch:", err)
		os.Exit(1)
	}

	allow := map[string]bool{}
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow[h] = true
		}
	}

	var existing map[string]bool
	if *additive && *out != "-" {
		existing = existingModelNames(*out)
	}

	result, err := Translate(providers, Opts{
		Allow:      allow,
		Version:    SourceVersion(),
		DraftAll:   *draft || *draftAllProviders,
		ProcessAll: *draftAllProviders,
		Existing:   existing,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "modelsdev-import: translate:", err)
		os.Exit(1)
	}

	// Emit the supported-hosts curation manifest (the promotion targets /
	// watcher allowlist) at the catalog repo root, outside the walked data
	// tree so it's never seeded or validated as a catalog kind.
	if *out != "-" {
		path := filepath.Join(*out, "..", "supported-hosts.yaml")
		if err := writeSupportedHosts(path, *hosts); err != nil {
			fmt.Fprintln(os.Stderr, "modelsdev-import: supported-hosts:", err)
		} else {
			fmt.Fprintln(os.Stderr, "modelsdev-import: wrote supported-hosts manifest →", path)
		}
	}

	if *out == "-" {
		emitStdout(result)
	} else {
		wr, _ := WriteToDisk(*out, result)
		fmt.Fprintf(os.Stderr, "modelsdev-import: wrote %d providers, %d hosts, %d models, %d bindings, %d pricings; preserved %d existing provider/host files (%d errors)\n",
			wr.Providers, wr.Hosts, wr.Models, wr.Bindings, wr.Pricings, wr.Preserved, wr.Errors)
	}

	if result.SkippedExisting > 0 {
		fmt.Fprintf(os.Stderr, "  skipped %d models already present in the catalog (additive)\n", result.SkippedExisting)
	}
	if result.SkippedNoBaseURL > 0 {
		fmt.Fprintf(os.Stderr, "  skipped %d allowlisted providers with no resolvable baseURL\n", result.SkippedNoBaseURL)
	}
	if len(result.UnsupportedNPM) > 0 {
		keys := make([]string, 0, len(result.UnsupportedNPM))
		for k := range result.UnsupportedNPM {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(os.Stderr, "  → drafts/ (no adapter for %s): %d provider(s)\n", k, result.UnsupportedNPM[k])
		}
	}
}

// writeSupportedHosts emits the curation manifest: the hosts the relay
// commits to supporting (promotion targets out of drafts/, and the watcher's
// allowlist), with each host's offered pricing strategies. It carries no
// apiVersion/kind and lives outside the data tree, so it is never seeded or
// validated as a catalog kind.
func writeSupportedHosts(path, hostsCSV string) error {
	type entry struct {
		ID                string   `yaml:"id"`
		PricingStrategies []string `yaml:"pricingStrategies"`
	}
	var hosts []entry
	for _, h := range strings.Split(hostsCSV, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, entry{ID: h, PricingStrategies: pricingStrategiesFor(h)})
		}
	}
	doc := struct {
		Description string  `yaml:"description"`
		Hosts       []entry `yaml:"hosts"`
	}{
		Description: "Hosts the relay commits to supporting — promotion targets out of drafts/ and the models.dev watcher allowlist. Not a seeded catalog kind. Generated by modelsdev-import -hosts=...",
		Hosts:       hosts,
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// existingModelNames returns the set of Model metadata.names already in the
// live catalog tree (manifest.LoadDir skips drafts/), so additive import can
// leave curated/imported entries untouched. Tolerant of a missing/empty dir.
func existingModelNames(dir string) map[string]bool {
	names := map[string]bool{}
	docs, err := manifest.LoadDir(dir)
	if err != nil {
		return names
	}
	for _, d := range docs {
		if d.Model != nil {
			names[d.Model.Metadata.Name] = true
		}
	}
	return names
}

func emitStdout(r *TranslateResult) {
	emit := func(v any) { b, _ := yaml.Marshal(v); fmt.Printf("---\n%s", b) }
	for _, p := range r.Providers {
		emit(p)
	}
	for _, h := range r.Hosts {
		emit(h)
	}
	for _, m := range r.Models {
		emit(m)
	}
	for _, b := range r.Bindings {
		emit(b)
	}
	for _, p := range r.Pricings {
		emit(p)
	}
}
