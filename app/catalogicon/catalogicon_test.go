package catalogicon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestContentType(t *testing.T) {
	for name, want := range map[string]string{
		"claude.png":    "image/png",
		"z-ai.svg":      "image/svg+xml",
		"meta.JPG":      "",
		"notes.txt":     "",
		"../claude.png": "",
		"a/claude.png":  "",
		".png":          "",
		"":              "",
	} {
		got, ok := ContentType(name)
		if got != want || ok != (want != "") {
			t.Errorf("ContentType(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
}

func TestReadDir(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := ReadDir(dir); found || err != nil {
		t.Fatalf("tree without icons: found %v, err %v", found, err)
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, Dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("claude.png", "png")
	write("notes.txt", "not an icon")
	write("huge.png", strings.Repeat("x", MaxBytes+1))
	write("nested/kimi.png", "png")

	icons, found, err := ReadDir(dir)
	if err != nil || !found {
		t.Fatalf("found %v, err %v", found, err)
	}
	if diff := cmp.Diff([]Icon{New("claude.png", []byte("png"))}, icons); diff != "" {
		t.Errorf("icons (-want +got):\n%s", diff)
	}
	if _, err := ReadFile(dir, "missing.png"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing icon: err %v, want fs.ErrNotExist", err)
	}
}
