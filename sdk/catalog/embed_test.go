package catalog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// The embedded catalog must index and resolve its own bindings: every pinned ref lands on the binding it names.
func TestLoad_EmbeddedResolvesItsOwnPins(t *testing.T) {
	ic, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if ic.Catalog.Version == "" || len(ic.Catalog.Hosts) == 0 {
		t.Fatalf("embedded catalog: version %q, %d hosts", ic.Catalog.Version, len(ic.Catalog.Hosts))
	}
	for _, h := range ic.Catalog.Hosts {
		for _, b := range h.Models {
			ref := b.MetadataName + "@" + h.Name
			if len(normRef(ref)) == maxRefLen {
				continue // a pin cut at the length limit can collide with another
			}
			got, host, err := ic.Resolve(ref)
			if err != nil || host.Name != h.Name || got.MetadataName != b.MetadataName {
				t.Errorf("Resolve(%q) = %q@%q, %v", ref, got.MetadataName, host.Name, err)
			}
		}
	}
}

func TestLoadBytesAndLoadFile(t *testing.T) {
	path := "/deploy/chat"
	want := &Catalog{
		Version: "relay-catalog@v1alpha2",
		Hosts: []Host{{
			Name: "h", BaseURL: "https://x", Path: &path, DisplayName: "H",
			Models: []Binding{{MetadataName: "m", Adapter: "openai", Name: "m-upstream", Providers: []string{"p"}, Featured: true, Aliases: []string{"m[*]"},
				Pricing: []Rate{{Meter: "tokens.input", Unit: "per_million", Amount: 2.5, AboveTokens: 200_000, ServiceTier: "priority"}}}},
		}},
		Models:    []ModelInfo{{MetadataName: "m", Provider: "p", Capabilities: Capabilities{Chat: true, ReasoningEfforts: []string{"low"}}, Modalities: Modalities{Input: []string{"text"}}}},
		Providers: []ProviderInfo{{Name: "p", DisplayName: "P"}},
	}
	plain, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(plain)
	_ = zw.Close()

	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	loads := map[string]func() (*IndexedCatalog, error){
		"LoadBytes":     func() (*IndexedCatalog, error) { return LoadBytes(plain) },
		"LoadFile json": func() (*IndexedCatalog, error) { return LoadFile(write("c.json", plain)) },
		"LoadFile gzip": func() (*IndexedCatalog, error) { return LoadFile(write("c.json.gz", zipped.Bytes())) },
	}
	for name, load := range loads {
		ic, err := load()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if diff := cmp.Diff(want, ic.Catalog); diff != "" {
			t.Errorf("%s: catalog (-want +got):\n%s", name, diff)
		}
		if b, h, err := ic.Resolve("p/m-upstream@h"); err != nil || b.MetadataName != "m" || h.Name != "h" {
			t.Errorf("%s: loaded catalog is not indexed: %v", name, err)
		}
	}

	for name, tc := range map[string]struct {
		load func() (*IndexedCatalog, error)
		want string
	}{
		"LoadBytes bad JSON":       {func() (*IndexedCatalog, error) { return LoadBytes([]byte("{")) }, "catalog: unmarshal: "},
		"LoadFile missing":         {func() (*IndexedCatalog, error) { return LoadFile(filepath.Join(dir, "absent.json")) }, "catalog: open "},
		"LoadFile bad JSON":        {func() (*IndexedCatalog, error) { return LoadFile(write("bad.json", []byte("nope"))) }, "catalog: unmarshal: "},
		"LoadFile truncated gzip":  {func() (*IndexedCatalog, error) { return LoadFile(write("cut.gz", zipped.Bytes()[:zipped.Len()-8])) }, "catalog: inflate "},
		"LoadFile bad gzip header": {func() (*IndexedCatalog, error) { return LoadFile(write("hdr.gz", []byte{0x1f, 0x8b, 0xff})) }, "catalog: inflate "},
	} {
		if ic, err := tc.load(); ic != nil || err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s = %v, %v; want an error starting %q", name, ic, err, tc.want)
		}
	}
}
