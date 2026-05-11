package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/identity"
	"github.com/wyolet/relay/internal/storage"
)

func runSeed(args []string) {
	if err := runSeedTo(os.Stdout, args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runSeedTo is the callable entry point (used by tests).
func runSeedTo(out io.Writer, args []string) error {
	var fromDir string
	var apply bool
	var dsn string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			if i+1 >= len(args) {
				return fmt.Errorf("--from requires a directory argument")
			}
			i++
			fromDir = args[i]
		case "--apply":
			apply = true
		case "--dsn":
			if i+1 >= len(args) {
				return fmt.Errorf("--dsn requires a connection string argument")
			}
			i++
			dsn = args[i]
		default:
			return fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	if fromDir == "" {
		return fmt.Errorf("--from <yaml-dir> is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if dsn == "" {
		dsn = cfg.PGDSN
	}
	if dsn == "" {
		return fmt.Errorf("RELAY_PG_DSN not set and --dsn not provided")
	}

	// Validate first — never open a transaction on bad input.
	src, err := catalog.LoadYAML(fromDir)
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}

	// Identity is parsed and resolved alongside the catalog. Postgres tables
	// for users do not exist yet — we report what was found so the schema
	// can be exercised before storage lands.
	idStore, err := identity.LoadYAML(fromDir)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	for _, u := range idStore.Users() {
		fmt.Fprintf(out, "User %q (username=%s, email=%s, password=%s)\n",
			u.Metadata.Name,
			u.Spec.Username.Source(),
			u.Spec.Email.Source(),
			u.Spec.Password.Source())
	}

	ctx := context.Background()

	st, err := storage.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer st.Close()

	pgStore, err := catalog.NewPGStoreNoReload(st.Catalog, st)
	if err != nil {
		return fmt.Errorf("pg store: %w", err)
	}
	if len(cfg.MasterKey) > 0 {
		pgStore.SetMasterKey(cfg.MasterKey)
	}

	diff, err := pgStore.Diff(ctx, src)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}

	printDiff(out, diff)

	if !apply {
		fmt.Fprintln(out, "\n(dry-run) pass --apply to write changes")
		return nil
	}

	if diff.Empty() {
		fmt.Fprintln(out, "\nno changes — skipping transaction")
		return nil
	}

	if err := pgStore.Seed(ctx, src); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	fmt.Fprintln(out, "\napplied")
	return nil
}

// maybeAutoSeed runs the seed path if every catalog table is empty.
func maybeAutoSeed(ctx context.Context, dsn, configDir string) error {
	st, err := storage.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer st.Close()

	pgStore, err := catalog.NewPGStoreNoReload(st.Catalog, st)
	if err != nil {
		return fmt.Errorf("pg store: %w", err)
	}

	empty, err := pgStore.IsEmpty(ctx)
	if err != nil {
		return fmt.Errorf("check empty: %w", err)
	}
	if !empty {
		return nil
	}

	src, err := catalog.LoadYAML(configDir)
	if err != nil {
		return fmt.Errorf("load yaml %s: %w", configDir, err)
	}

	diff, err := pgStore.Diff(ctx, src)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}

	if diff.Empty() {
		return nil
	}

	if err := pgStore.Seed(ctx, src); err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	total := len(src.Providers()) + len(src.Policies()) + len(src.Secrets()) +
		len(src.Models()) + len(src.Routes()) + len(src.RateLimits())
	slog.Info("auto-seed: applied", "rows", total, "dir", configDir)
	return nil
}

func printDiff(out io.Writer, d *catalog.SeedDiff) {
	for _, kd := range []catalog.KindDiff{
		d.Providers, d.Policies, d.Secrets, d.Models, d.Routes, d.RateLimits,
	} {
		if kd.Empty() {
			continue
		}
		fmt.Fprintf(out, "%s:\n", kd.Kind)
		for _, n := range kd.Creates {
			fmt.Fprintf(out, "  + %s\n", n)
		}
		for _, n := range kd.Updates {
			fmt.Fprintf(out, "  ~ %s\n", n)
		}
		for _, n := range kd.Deletes {
			fmt.Fprintf(out, "  - %s\n", n)
		}
	}
}
