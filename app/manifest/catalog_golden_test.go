package manifest_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/embed"
	catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"
)

var updateCatalogGolden = flag.Bool("update-catalog-golden", false, "rewrite testdata/catalog_golden.json from $RELAY_CATALOG_DIR")

const catalogGoldenPath = "testdata/catalog_golden.json"

// catalogGolden pins what parsing, translating, rendering and embedding one catalog tree produced, as digests of canonical JSON plus a few readable fields.
type catalogGolden struct {
	Tree       string            `json:"tree"`
	Kinds      map[string]int    `json:"kinds"`
	First      map[string]string `json:"first"`
	Parsed     string            `json:"parsed"`
	Translated string            `json:"translated"`
	Rendered   string            `json:"rendered"`
	Embed      string            `json:"embed"`
}

type translatedDoc struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Value any    `json:"value,omitempty"`
	Err   string `json:"err,omitempty"`
}

// TestCatalogTreeGolden reads a real catalog tree through relay's parser and through the catalog-only one, and holds both to the golden.
func TestCatalogTreeGolden(t *testing.T) {
	dir := os.Getenv("RELAY_CATALOG_DIR")
	if dir == "" {
		t.Skip("RELAY_CATALOG_DIR unset")
	}
	tree := treeDigest(t, dir)
	docs, err := manifest.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	catalogDocs, err := catalogmanifest.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	got := catalogGolden{Tree: tree, Kinds: map[string]int{}, First: map[string]string{}}
	var payloads []any
	for _, d := range docs {
		got.Kinds[d.Kind()]++
		if _, ok := got.First[d.Kind()]; !ok {
			got.First[d.Kind()] = d.Meta().Name
		}
		payloads = append(payloads, d.Payload())
	}
	got.Parsed = jsonDigest(t, payloads)
	got.Translated = jsonDigest(t, translateAll(docs, relayTranslate))
	rendered, err := manifest.Render(docs, "")
	if err != nil {
		t.Fatal(err)
	}
	got.Rendered = digest(rendered)
	cat, err := embed.Compose(catalogDocs, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := embed.MarshalJSON(cat)
	if err != nil {
		t.Fatal(err)
	}
	got.Embed = digest(embedded)

	if *updateCatalogGolden {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(catalogGoldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(catalogGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var want catalogGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if want.Tree != tree {
		t.Skipf("RELAY_CATALOG_DIR holds a different tree (%s) than the golden was taken from (%s)", tree, want.Tree)
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("catalog tree output changed:\n got %s\nwant %s", g, w)
	}

	// The catalog-only parser must yield the same documents and translations.
	lifted := make([]manifest.Document, len(catalogDocs))
	catalogPayloads := make([]any, len(catalogDocs))
	for i, d := range catalogDocs {
		lifted[i] = manifest.Document{Provider: d.Provider, Host: d.Host, Model: d.Model, Policy: d.Policy,
			RateLimit: d.RateLimit, Pricing: d.Pricing, HostBinding: d.HostBinding}
		catalogPayloads[i] = lifted[i].Payload()
	}
	if p := jsonDigest(t, catalogPayloads); p != want.Parsed {
		t.Errorf("catalog parser: parsed digest %s, want %s", p, want.Parsed)
	}
	if tr := jsonDigest(t, translateAll(lifted, catalogTranslate)); tr != want.Translated {
		t.Errorf("catalog parser: translated digest %s, want %s", tr, want.Translated)
	}
}

type translateFunc func(d manifest.Document, idx manifest.MapResolver) (any, error)

func relayTranslate(d manifest.Document, idx manifest.MapResolver) (any, error) {
	switch {
	case d.Provider != nil:
		return manifest.ToProvider(*d.Provider, idx)
	case d.Host != nil:
		return manifest.ToHost(*d.Host, idx)
	case d.Model != nil:
		return manifest.ToModel(*d.Model, idx)
	case d.Policy != nil:
		return manifest.ToPolicy(*d.Policy, idx)
	case d.RateLimit != nil:
		return manifest.ToRateLimit(*d.RateLimit, idx)
	case d.Pricing != nil:
		return manifest.ToPricing(*d.Pricing, idx)
	case d.HostBinding != nil:
		return manifest.ToHostBinding(*d.HostBinding, idx)
	}
	return nil, nil
}

func catalogTranslate(d manifest.Document, m manifest.MapResolver) (any, error) {
	idx := catalogmanifest.MapResolver{Providers: m.Providers, Hosts: m.Hosts, Policies: m.Policies,
		Models: m.Models, RateLimits: m.RateLimits, Pricings: m.Pricings, Bindings: m.Bindings}
	switch {
	case d.Provider != nil:
		return catalogmanifest.ToProvider(*d.Provider, idx)
	case d.Host != nil:
		return catalogmanifest.ToHost(*d.Host, idx)
	case d.Model != nil:
		return catalogmanifest.ToModel(*d.Model, idx)
	case d.Policy != nil:
		return catalogmanifest.ToPolicy(*d.Policy, idx)
	case d.RateLimit != nil:
		return catalogmanifest.ToRateLimit(*d.RateLimit, idx)
	case d.Pricing != nil:
		return catalogmanifest.ToPricing(*d.Pricing, idx)
	case d.HostBinding != nil:
		return catalogmanifest.ToHostBinding(*d.HostBinding, idx)
	}
	return nil, nil
}

// translateAll translates every document against ids derived from names, so the output is stable across runs.
func translateAll(docs []manifest.Document, translate translateFunc) []translatedDoc {
	idx := manifest.MapResolver{
		Providers: map[string]string{}, Hosts: map[string]string{}, Policies: map[string]string{},
		Models: map[string]string{}, RateLimits: map[string]string{}, Pricings: map[string]string{},
		Bindings: map[string]string{},
	}
	for _, d := range docs {
		name := d.Meta().Name
		id := d.Kind() + "/" + name
		switch {
		case d.Provider != nil:
			idx.Providers[name] = id
		case d.Host != nil:
			idx.Hosts[name] = id
		case d.Policy != nil:
			idx.Policies[name] = id
		case d.Model != nil:
			idx.Models[name] = id
		case d.RateLimit != nil:
			idx.RateLimits[name] = id
		case d.Pricing != nil:
			idx.Pricings[name] = id
		case d.HostBinding != nil:
			idx.Bindings[name] = id
		}
	}
	out := make([]translatedDoc, 0, len(docs))
	for _, d := range docs {
		v, err := translate(d, idx)
		td := translatedDoc{Kind: d.Kind(), Name: d.Meta().Name, Value: v}
		if err != nil {
			td.Value, td.Err = nil, err.Error()
		}
		out = append(out, td)
	}
	return out
}

// treeDigest hashes the path and bytes of every file LoadDir reads under dir.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "drafts" {
				return fs.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(d.Name())
		if (ext != ".yaml" && ext != ".yml") || strings.HasSuffix(d.Name(), ".draft"+ext) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(filepath.ToSlash(rel) + "\x00"))
		h.Write(b)
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func jsonDigest(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return digest(b)
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
