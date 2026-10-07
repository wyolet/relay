package modeldir

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
)

const handWritten = `# served by a local box; kept by hand
name: local-llm
provider: local
adapter: openai
pricing:
  - {meter: tokens.input, unit: per_million, amount: 0}
`

func writeHandWritten(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "local-llm.yaml"), []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMatchesSource(t *testing.T) {
	dir := t.TempDir()
	srcCatalog := fixtureCatalog("v0.1.10")
	src := fixtureSource(t, srcCatalog)
	ctx := context.Background()
	var added []Model
	for _, ref := range []string{"claude-opus-5-5", "gpt-5.5-2026-04-23", "llama-4-maverick", "mixtral-8x22b"} {
		m, err := Add(ctx, dir, ref, src, AddOptions{})
		if err != nil {
			t.Fatal(err)
		}
		added = append(added, m)
	}
	writeHandWritten(t, dir)

	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	full, err := src.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tokens := usage.Tokens{"input": 250000, "output": 1000, "cache_read": 5000}
	for _, m := range added {
		pin := m.Name + "@" + m.PricedBy
		wantB, wantH, err := full.Resolve(pin)
		if err != nil {
			t.Fatal(err)
		}
		wantCost, wantUnpriced, wantOK := full.Cost(pin, tokens)
		refs := append([]string{m.Name, m.WireName, m.Provider + "/" + m.Name, pin}, m.Aliases...)
		for _, ref := range refs {
			b, h, err := loaded.Resolve(ref)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", ref, err)
			}
			if b.Name != wantB.Name || b.MetadataName != wantB.MetadataName || b.Adapter != wantB.Adapter ||
				!reflect.DeepEqual(b.Pricing, wantB.Pricing) || h.Name != wantH.Name {
				t.Fatalf("Resolve(%q) = %+v @ %s, want %+v @ %s", ref, b, h.Name, wantB, wantH.Name)
			}
			cost, unpriced, ok := loaded.Cost(ref, tokens)
			if cost != wantCost || ok != wantOK || !reflect.DeepEqual(unpriced, wantUnpriced) {
				t.Fatalf("Cost(%q) = %v %v %v, want %v %v %v", ref, cost, unpriced, ok, wantCost, wantUnpriced, wantOK)
			}
		}
	}

	if len(loaded.Catalog.Models) != 5 {
		t.Fatalf("models = %d, want 5", len(loaded.Catalog.Models))
	}
	for _, info := range loaded.Catalog.Models {
		for _, want := range srcCatalog.Models {
			if want.MetadataName == info.MetadataName && !reflect.DeepEqual(info, want) {
				t.Fatalf("model info %s = %+v, want %+v", info.MetadataName, info, want)
			}
		}
	}
	if b, _, err := loaded.Resolve("local-llm"); err != nil || b.Name != "local-llm" || b.Adapter != "openai" {
		t.Fatalf("hand-written: %+v, %v", b, err)
	}
}

func TestLoadEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	ic, err := Load(missing)
	if !errors.Is(err, ErrNoModels) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir: err = %v", err)
	}
	if ic == nil || len(ic.Catalog.Hosts) != 0 {
		t.Fatalf("missing dir: catalog = %+v, want empty", ic)
	}
	if _, _, err := ic.Resolve("claude-opus-5-5"); err == nil {
		t.Fatal("empty catalog resolved a model")
	}

	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, "README.md"), []byte("models"), 0o644); err != nil {
		t.Fatal(err)
	}
	ic, err = Load(empty)
	if !errors.Is(err, ErrNoModels) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty dir: err = %v", err)
	}
	if ic == nil || len(ic.Catalog.Hosts) != 0 {
		t.Fatalf("empty dir: catalog = %+v, want empty", ic)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"typo key":      "name: bad\npriceBy: anthropic\n",
		"name mismatch": "name: other\n",
		"empty":         "",
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if ic, err := Load(dir); err == nil || ic != nil || errors.Is(err, ErrNoModels) {
				t.Fatalf("Load = %v, %v; want nil and a file error", ic, err)
			}
		})
	}
}
