package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestBuild_MCPTarballOnlyForLiveMCPTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	commit := func(files map[string]string) {
		t.Helper()
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
			{"add", "-A"},
			{"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "data"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	build1 := func() (int, []string) {
		t.Helper()
		out := t.TempDir()
		code := build([]string{"--repo", repo, "--ref", "HEAD", "--version", "v0.1.0", "--out", out})
		entries, err := os.ReadDir(out)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return code, names
	}
	relayFiles := []string{"catalog.json.gz", "catalog.json.gz.sha256", "relay-catalog-data-v0.1.0.tar.gz", "relay-catalog-data-v0.1.0.tar.gz.sha256"}

	commit(map[string]string{"data/providers/acme/provider.yaml": "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Provider\nmetadata:\n  name: acme\n  displayName: Acme\nspec:\n  enabled: true\n"})
	if code, names := build1(); code != 0 || !slices.Equal(names, relayFiles) {
		t.Fatalf("no mcp/: exit %d, files %v", code, names)
	}

	commit(map[string]string{"mcp/drafts/wip.yaml": "apiVersion: gateway.example.com/v1alpha1\nkind: MCPTemplate\n"})
	if code, names := build1(); code != 0 || !slices.Equal(names, relayFiles) {
		t.Fatalf("mcp/ with drafts only: exit %d, files %v", code, names)
	}

	commit(map[string]string{"mcp/servers/wiki.yaml": "apiVersion: gateway.example.com/v1alpha1\nkind: MCPTemplate\nmetadata:\n  name: wiki\n"})
	want := append(slices.Clone(relayFiles), "mcp-catalog-data-v0.1.0.tar.gz", "mcp-catalog-data-v0.1.0.tar.gz.sha256")
	slices.Sort(want)
	if code, names := build1(); code != 0 || !slices.Equal(names, want) {
		t.Fatalf("live mcp/: exit %d, files %v", code, names)
	}

	commit(map[string]string{"mcp/servers/other.yaml": "apiVersion: gateway.example.com/v1beta1\nkind: MCPTemplate\n"})
	if code, _ := build1(); code != 1 {
		t.Fatalf("mcp/ documents on two apiVersions: exit %d, want 1", code)
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
	for _, k := range []string{"ADDR", "REPO", "RELEASES_URL", "SYNC_INTERVAL", "CACHE_BYTES", "MIRROR_DIR", "WEBHOOKS", "WEBHOOK_ALLOW_LOOPBACK"} {
		t.Setenv("RELAY_CATALOG_SERVER_"+k, "")
	}
	cfg, err := loadServeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != ":8080" || cfg.SyncInterval != time.Minute || cfg.CacheBytes != 64<<20 ||
		cfg.RepoURL != "https://github.com/wyolet/relay-catalog.git" ||
		cfg.ReleasesURL != "https://github.com/wyolet/relay-catalog/releases/download" ||
		cfg.DisableWebhooks || cfg.AllowLoopbackWebhooks {
		t.Fatalf("defaults %+v", cfg)
	}
	t.Setenv("RELAY_CATALOG_SERVER_WEBHOOKS", "off")
	t.Setenv("RELAY_CATALOG_SERVER_WEBHOOK_ALLOW_LOOPBACK", "on")
	if cfg, err = loadServeConfig(); err != nil || !cfg.DisableWebhooks || !cfg.AllowLoopbackWebhooks {
		t.Fatalf("switches: %+v, %v", cfg, err)
	}
	t.Setenv("RELAY_CATALOG_SERVER_WEBHOOKS", "maybe")
	if _, err := loadServeConfig(); err == nil {
		t.Fatal("an unknown switch value accepted")
	}
	t.Setenv("RELAY_CATALOG_SERVER_WEBHOOKS", "")
	t.Setenv("RELAY_CATALOG_SERVER_SYNC_INTERVAL", "0s")
	if _, err := loadServeConfig(); err == nil {
		t.Fatal("zero sync interval accepted")
	}
}
