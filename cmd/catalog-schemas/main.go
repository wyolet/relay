// catalog-schemas dumps JSON Schema files for every catalog wire kind,
// one file per kind under <out>/. The schemas are derived from the same
// Go types (app/manifest DTOs) the relay binary uses to parse YAMLs, so
// they stay in lockstep with the code by construction.
//
// Usage:
//
//	catalog-schemas <out-dir>
//
// Produces, e.g.:
//
//	<out>/Provider.schema.json
//	<out>/Host.schema.json
//	<out>/Model.schema.json
//	... and so on for every catalog kind.
//
// Each file is self-contained: the root schema is the kind wrapper
// (apiVersion+kind+metadata+spec), and any referenced sub-types are
// inlined under $defs. Editors that understand the
// `# yaml-language-server: $schema=...` directive get autocomplete and
// inline diagnostics. CI uses check-jsonschema against the same files.
//
// Run `make schemas` to regenerate; CI ensures `git diff --exit-code
// schemas/` so Go-type changes that affect the wire format land with
// their corresponding schema bump.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/manifest"
)

const (
	apiVersion    = "v1alpha2"
	schemaIDBase  = "https://relay-api.wyolet.dev/schemas/" + apiVersion + "/"
	draft         = "https://json-schema.org/draft/2020-12/schema"
	refPrefix     = "#/$defs/"
	humaRefPrefix = "#/components/schemas/"
)

// kind is the entry-shape we want a top-level schema file for.
type kind struct {
	Name string       // schema filename + json title
	Type reflect.Type // the Go wire DTO
}

var kinds = []kind{
	{Name: "Provider", Type: reflect.TypeOf(manifest.ProviderDTO{})},
	{Name: "Host", Type: reflect.TypeOf(manifest.HostDTO{})},
	{Name: "Model", Type: reflect.TypeOf(manifest.ModelDTO{})},
	{Name: "HostKey", Type: reflect.TypeOf(manifest.HostKeyDTO{})},
	{Name: "Policy", Type: reflect.TypeOf(manifest.PolicyDTO{})},
	{Name: "RateLimit", Type: reflect.TypeOf(manifest.RateLimitDTO{})},
	{Name: "Pricing", Type: reflect.TypeOf(manifest.PricingDTO{})},
	{Name: "RelayKey", Type: reflect.TypeOf(manifest.RelayKeyDTO{})},
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: catalog-schemas <out-dir>")
		os.Exit(2)
	}
	out := os.Args[1]
	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", out, err)
		os.Exit(2)
	}

	for _, k := range kinds {
		if err := dumpOne(out, k); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", k.Name, err)
			os.Exit(1)
		}
	}
}

// dumpOne builds one self-contained schema file for kind k. A fresh huma
// registry is used per kind so $defs contain only what this kind references
// (no cross-kind bleed). Refs emitted by huma point at
// #/components/schemas/X — we rewrite them to #/$defs/X to match
// JSON Schema's $defs convention.
func dumpOne(out string, k kind) error {
	reg := huma.NewMapRegistry(humaRefPrefix, huma.DefaultSchemaNamer)
	root := huma.SchemaFromType(reg, k.Type)
	defs := map[string]any{}
	// The registry's Map() returns the types we transitively referenced.
	for name, s := range reg.Map() {
		if name == k.Name {
			// Top-level kind goes at the root, not under $defs.
			continue
		}
		// Marshal/unmarshal through generic map so we can rewrite $refs
		// without depending on huma's internal Schema layout.
		var generic map[string]any
		b, err := json.Marshal(s)
		if err != nil {
			return fmt.Errorf("marshal def %s: %w", name, err)
		}
		if err := json.Unmarshal(b, &generic); err != nil {
			return fmt.Errorf("unmarshal def %s: %w", name, err)
		}
		rewriteRefs(generic)
		defs[name] = generic
	}

	var rootGeneric map[string]any
	rootBytes, err := json.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal root: %w", err)
	}
	if err := json.Unmarshal(rootBytes, &rootGeneric); err != nil {
		return fmt.Errorf("unmarshal root: %w", err)
	}
	rewriteRefs(rootGeneric)

	doc := map[string]any{
		"$schema": draft,
		"$id":     schemaIDBase + k.Name + ".schema.json",
		"title":   k.Name,
	}
	for kk, vv := range rootGeneric {
		doc[kk] = vv
	}
	if len(defs) > 0 {
		doc["$defs"] = defs
	}

	path := filepath.Join(out, k.Name+".schema.json")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", path)
	return nil
}

// rewriteRefs walks v in place, replacing every "$ref" value that begins
// with humaRefPrefix with the equivalent refPrefix path. Recurses into
// arbitrary JSON-shaped maps and slices.
func rewriteRefs(v any) {
	switch x := v.(type) {
	case map[string]any:
		for key, val := range x {
			if key == "$ref" {
				if s, ok := val.(string); ok && len(s) > len(humaRefPrefix) && s[:len(humaRefPrefix)] == humaRefPrefix {
					x[key] = refPrefix + s[len(humaRefPrefix):]
				}
				continue
			}
			rewriteRefs(val)
		}
	case []any:
		for _, el := range x {
			rewriteRefs(el)
		}
	}
}
