package catalogsource

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
)

func fixtureCatalog(version string) *catalog.Catalog {
	return &catalog.Catalog{
		Version: version,
		Hosts: []catalog.Host{
			{Name: "anthropic", BaseURL: "https://anthropic.example", Models: []catalog.Binding{
				{Name: "claude-opus-5-5", MetadataName: "claude-opus-5-5", Adapter: "anthropic", Providers: []string{"anthropic"}},
			}},
		},
		Models: []catalog.ModelInfo{{MetadataName: "claude-opus-5-5", Provider: "anthropic", DisplayName: "Claude Opus 5.5"}},
	}
}

func catalogJSON(t *testing.T, c *catalog.Catalog) []byte {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFileReadsPlainAndGzip(t *testing.T) {
	dir := t.TempDir()
	data := catalogJSON(t, fixtureCatalog("v0.1.10"))
	for name, body := range map[string][]byte{"catalog.json": data, "catalog.json.gz": gzipped(t, data)} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		ic, err := File(path).Catalog(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ic.Resolve("claude-opus-5-5@anthropic"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
