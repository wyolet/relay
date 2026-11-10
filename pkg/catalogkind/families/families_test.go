package families

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLineNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "families.yaml")
	body := "families:\n" +
		"    - name: gpt\n" +
		"      aliases: [o]\n" +
		"      lines:\n" +
		"        - name: o\n" +
		"          aliases: [o-mini]\n" +
		"        - name: codex\n" +
		"    - name: claude\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if gpt := f.Families[0]; len(gpt.Aliases) != 1 || len(gpt.Lines) != 2 || gpt.Lines[0].Aliases[0] != "o-mini" {
		t.Fatalf("gpt = %+v", gpt)
	}
	got := f.LineNames()
	if _, ok := got["o"]; ok || len(got) != 2 {
		t.Fatalf("families = %v", got)
	}
	if gpt := got["gpt"]; !gpt["o"] || !gpt["codex"] || gpt["o-mini"] || len(gpt) != 2 {
		t.Fatalf("gpt lines = %v", gpt)
	}
	if claude, ok := got["claude"]; !ok || len(claude) != 0 {
		t.Fatalf("claude lines = %v", claude)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	var pe *fs.PathError
	if _, err := Load(filepath.Join(dir, "missing.yaml")); !errors.As(err, &pe) {
		t.Fatalf("missing file: err = %v, want *fs.PathError", err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("families: {"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || errors.As(err, &pe) {
		t.Fatalf("bad yaml: err = %v, want a parse error", err)
	}
}
