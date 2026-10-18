package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBuild_WritesFilesWithSums(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	files := map[string]string{
		"data/providers/acme/provider.yaml": "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Provider\nmetadata:\n  name: acme\n  displayName: Acme\nspec:\n  enabled: true\n",
		"README.md":                         "tooling\n",
	}
	for name, body := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"add", "-A"},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "data"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_COMMITTER_DATE="+time.Now().Format(time.RFC3339))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	out := filepath.Join(t.TempDir(), "out")
	if code := build([]string{"--repo", repo, "--ref", "HEAD", "--version", "v0.1.0", "--out", out}); code != 0 {
		t.Fatalf("build exit %d", code)
	}
	for _, name := range []string{"relay-catalog-data-v0.1.0.tar.gz", "catalog.json.gz"} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		sum, err := os.ReadFile(filepath.Join(out, name+".sha256"))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(data)
		if want := hex.EncodeToString(h[:]) + "  " + name + "\n"; string(sum) != want {
			t.Fatalf("%s.sha256 = %q, want %q", name, sum, want)
		}
	}

	if code := build([]string{"--repo", repo, "--ref", "HEAD", "--version", "../x", "--out", out}); code != 2 {
		t.Fatalf("path-like version: exit %d, want 2", code)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"67108864": 64 << 20, "64Mi": 64 << 20, "512Ki": 512 << 10, "1Gi": 1 << 30} {
		if got, err := parseBytes(in); err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "64MB", "Mi", "99999999999Gi"} {
		if _, err := parseBytes(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestLoadServeConfig(t *testing.T) {
	for _, k := range []string{"ADDR", "REPO", "RELEASES_URL", "SYNC_INTERVAL", "CACHE_BYTES", "MIRROR_DIR"} {
		t.Setenv("RELAY_CATALOG_SERVER_"+k, "")
	}
	cfg, err := loadServeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != ":8080" || cfg.SyncInterval != time.Minute || cfg.CacheBytes != 64<<20 ||
		cfg.RepoURL != "https://github.com/wyolet/relay-catalog.git" ||
		cfg.ReleasesURL != "https://github.com/wyolet/relay-catalog/releases/download" {
		t.Fatalf("defaults %+v", cfg)
	}
	t.Setenv("RELAY_CATALOG_SERVER_SYNC_INTERVAL", "0s")
	if _, err := loadServeConfig(); err == nil {
		t.Fatal("zero sync interval accepted")
	}
}
