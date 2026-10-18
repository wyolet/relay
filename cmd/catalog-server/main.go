// Command catalog-server serves relay-catalog releases from a git mirror
// (serve) and builds one tag's release files (build).
//
// Usage:
//
//	catalog-server serve
//	catalog-server build --repo <dir> --ref <commit> --version <tag> --out <dir>
//
// serve reads its configuration from RELAY_CATALOG_SERVER_* environment
// variables; see docs/reference/catalog-server.mdx.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wyolet/relay/app/catalogserver"
)

const usage = `usage:
  catalog-server serve
  catalog-server build --repo <dir> --ref <commit> --version <tag> --out <dir>`

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve())
	case "build":
		os.Exit(build(os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

type serveConfig struct {
	addr string
	catalogserver.Config
}

func loadServeConfig() (serveConfig, error) {
	cfg := serveConfig{
		addr: envOr("RELAY_CATALOG_SERVER_ADDR", ":8080"),
		Config: catalogserver.Config{
			RepoURL:     envOr("RELAY_CATALOG_SERVER_REPO", "https://github.com/wyolet/relay-catalog.git"),
			ReleasesURL: envOr("RELAY_CATALOG_SERVER_RELEASES_URL", "https://github.com/wyolet/relay-catalog/releases/download"),
			MirrorDir:   envOr("RELAY_CATALOG_SERVER_MIRROR_DIR", filepath.Join(os.TempDir(), "relay-catalog-mirror")),
		},
	}
	var err error
	if cfg.SyncInterval, err = time.ParseDuration(envOr("RELAY_CATALOG_SERVER_SYNC_INTERVAL", "60s")); err != nil || cfg.SyncInterval <= 0 {
		return cfg, fmt.Errorf("RELAY_CATALOG_SERVER_SYNC_INTERVAL: want a positive duration, got %q", os.Getenv("RELAY_CATALOG_SERVER_SYNC_INTERVAL"))
	}
	if cfg.CacheBytes, err = parseBytes(envOr("RELAY_CATALOG_SERVER_CACHE_BYTES", "64Mi")); err != nil {
		return cfg, fmt.Errorf("RELAY_CATALOG_SERVER_CACHE_BYTES: %w", err)
	}
	return cfg, nil
}

func serve() int {
	cfg, err := loadServeConfig()
	if err != nil {
		slog.Error("catalog-server: config", "err", err)
		return 2
	}
	srv, err := catalogserver.New(cfg.Config)
	if err != nil {
		slog.Error("catalog-server: config", "err", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	syncErr := make(chan error, 1)
	go func() { syncErr <- srv.Run(ctx) }()

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// A cold request may wait for a build or a fetch of the published file.
		WriteTimeout:   3 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
	}
	httpErr := make(chan error, 1)
	go func() { httpErr <- httpSrv.ListenAndServe() }()
	slog.Info("catalog-server: listening", "addr", cfg.addr, "repo", cfg.RepoURL, "mirror", cfg.MirrorDir)

	code := 0
	select {
	case <-ctx.Done():
		slog.Info("catalog-server: shutting down")
	case err := <-httpErr:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("catalog-server: http", "err", err)
			code = 1
		}
	case err := <-syncErr:
		if err != nil {
			slog.Error("catalog-server: sync", "err", err)
			code = 1
		}
	}
	stop()
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	return code
}

func build(args []string) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	repoDir := fs.String("repo", ".", "catalog repository (bare or checkout)")
	ref := fs.String("ref", "", "commit to build (required)")
	version := fs.String("version", "", "release tag, recorded as the catalog version (required)")
	out := fs.String("out", "", "output directory (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *ref == "" || *out == "" || *version == "" || strings.ContainsAny(*version, `/\`) || strings.HasPrefix(*version, ".") {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	ctx := context.Background()
	repo := catalogserver.Repo{Dir: *repoDir}
	commit, committed, err := repo.Commit(ctx, *ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve %s: %v\n", *ref, err)
		return 1
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// The data tarball is written before compose runs: it never depends on it.
	archive, err := catalogserver.BuildData(ctx, repo, commit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "data tarball: %v\n", err)
		return 1
	}
	if err := writeWithSum(*out, catalogserver.DataAsset(*version), archive); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cat, err := catalogserver.BuildCatalog(archive, committed, *version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", catalogserver.CatalogAsset, err)
		return 1
	}
	if err := writeWithSum(*out, catalogserver.CatalogAsset, cat); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func writeWithSum(dir, name string, data []byte) error {
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".sha256"), catalogserver.SumFile(name, data), 0o644)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// parseBytes reads a byte count, optionally with a Ki, Mi or Gi suffix.
func parseBytes(s string) (int64, error) {
	digits, mult := s, int64(1)
	for suffix, m := range map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30} {
		if v, ok := strings.CutSuffix(s, suffix); ok {
			digits, mult = v, m
			break
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("want a positive byte count such as 67108864 or 64Mi, got %q", s)
	}
	return n * mult, nil
}

// logLevel reads RELAY_LOG_LEVEL (debug|info|warn|error, default info).
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
