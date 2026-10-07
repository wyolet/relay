package modeldir

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
)

// snapshotDir returns every file's bytes keyed by name.
func snapshotDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		out[e.Name()] = readFile(t, filepath.Join(dir, e.Name()))
	}
	return out
}

func addAll(t *testing.T, dir string, src Source, refs ...string) {
	t.Helper()
	for _, ref := range refs {
		if _, err := Add(context.Background(), dir, ref, src, AddOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func kinds(changes []Change) map[string]ChangeKind {
	out := map[string]ChangeKind{}
	for _, c := range changes {
		out[c.Name] = c.Kind
	}
	return out
}

func TestRefreshNoChangeIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	src := fixtureSource(t, fixtureCatalog("v0.1.10"))
	addAll(t, dir, src, "claude-opus-5-5", "gpt-5-5-2026-04-23", "llama-4-maverick")
	if _, err := Add(context.Background(), dir, "mixtral-8x22b", src, AddOptions{PricedBy: "groq"}); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, dir)

	for range 2 {
		changes, err := Refresh(context.Background(), dir, src)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 4 {
			t.Fatalf("changes = %+v", changes)
		}
		for _, c := range changes {
			if c.Kind != Unchanged {
				t.Fatalf("%s: %s %v", c.Name, c.Kind, c.Fields)
			}
		}
		if after := snapshotDir(t, dir); !reflect.DeepEqual(before, after) {
			t.Fatal("refresh with no catalog change rewrote files")
		}
	}
}

func TestRefreshUpdatesMissingAndHandWritten(t *testing.T) {
	dir := t.TempDir()
	v1 := fixtureSource(t, fixtureCatalog("v0.1.10"))
	addAll(t, dir, v1, "claude-opus-5-5", "gpt-5-5-2026-04-23")
	if _, err := Add(context.Background(), dir, "mixtral-8x22b", v1, AddOptions{PricedBy: "groq"}); err != nil {
		t.Fatal(err)
	}
	writeHandWritten(t, dir)
	// A hand-written file named like a catalog model is still the user's.
	handLlama := []byte("name: llama-4-maverick\npricing:\n  - {meter: tokens.input, unit: per_million, amount: 99}\n")
	if err := os.WriteFile(filepath.Join(dir, "llama-4-maverick.yaml"), handLlama, 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, dir)

	next := fixtureCatalog("v0.1.11")
	next.Hosts[1].Models[0].Pricing[2].Amount = 30 // anthropic claude output price
	var groq []catalog.Binding
	for _, b := range next.Hosts[2].Models {
		if b.MetadataName != "mixtral-8x22b" {
			groq = append(groq, b)
		}
	}
	next.Hosts[2].Models = groq // mixtral stays at openrouter only; the file is priced by groq
	changes, err := Refresh(context.Background(), dir, fixtureSource(t, next))
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]ChangeKind{"claude-opus-5-5": Updated, "gpt-5-5-2026-04-23": Unchanged, "mixtral-8x22b": Missing}
	if got := kinds(changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	after := snapshotDir(t, dir)
	for _, name := range []string{"gpt-5-5-2026-04-23.yaml", "mixtral-8x22b.yaml", "local-llm.yaml", "llama-4-maverick.yaml"} {
		if !bytes.Equal(before[name], after[name]) {
			t.Fatalf("%s changed:\n%s", name, after[name])
		}
	}
	for _, c := range changes {
		if c.Name != "claude-opus-5-5" {
			continue
		}
		if !reflect.DeepEqual(c.Fields, []string{"pricing"}) {
			t.Fatalf("fields = %v", c.Fields)
		}
		if c.Before.Pricing[2].Amount != 25 || c.After.Pricing[2].Amount != 30 || c.After.Source.Catalog != "v0.1.11" {
			t.Fatalf("before/after = %+v / %+v", c.Before.Pricing, c.After)
		}
	}
	m, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m {
		if f.Name == "claude-opus-5-5" && (f.Pricing[2].Amount != 30 || f.Source.Catalog != "v0.1.11" || f.PricedBy != "anthropic") {
			t.Fatalf("claude file after refresh = %+v", f)
		}
	}
}

func TestRefreshSkipsSourceWithoutDerivedFiles(t *testing.T) {
	dir := t.TempDir()
	writeHandWritten(t, dir)
	changes, err := Refresh(context.Background(), dir, failingSource{})
	if err != nil || changes != nil {
		t.Fatalf("Refresh = %v, %v; want no changes and no source fetch", changes, err)
	}
}

type failingSource struct{}

func (failingSource) Catalog(context.Context) (*catalog.IndexedCatalog, error) {
	return nil, os.ErrPermission
}
