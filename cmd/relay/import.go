package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/internal/catalog"
	litellmimport "github.com/wyolet/relay/internal/import/litellm"
	storagemod "github.com/wyolet/relay/internal/storage"
)

func runImport(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: relay import <source> [flags]")
		fmt.Fprintln(os.Stderr, "  sources: litellm")
		os.Exit(1)
	}

	switch args[0] {
	case "litellm":
		runImportLiteLLM(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "import: source %q not yet supported\n", args[0])
		os.Exit(1)
	}
}

func runImportLiteLLM(args []string) {
	fs := flag.NewFlagSet("import litellm", flag.ExitOnError)
	apply := fs.Bool("apply", false, "Write to storage. Without this flag, prints YAML to stdout (dry-run).")
	mode := fs.String("mode", "upsert", "How to handle existing entries: upsert | skip-existing | overwrite")
	providers := fs.String("providers", "", "Comma-separated litellm_provider values to include (default: all)")
	models := fs.String("models", "", "Regex to filter model names (default: all)")
	sourceURL := fs.String("source-url", litellmimport.DefaultLiteLLMURL, "Override the LiteLLM JSON URL")
	sourceFile := fs.String("source-file", "", "Read JSON from a local file instead of fetching")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	ctx := context.Background()

	// Fetch.
	slog.Info("import litellm: fetching", "source", sourceFileOrURL(*sourceFile, *sourceURL))
	entries, version, err := litellmimport.Fetch(ctx, *sourceURL, *sourceFile)
	if err != nil {
		slog.Error("import litellm: fetch failed", "err", err)
		os.Exit(1)
	}
	slog.Info("import litellm: fetched", "entries", len(entries), "version", version)

	// Filter by --providers.
	if *providers != "" {
		want := map[string]bool{}
		for _, p := range strings.Split(*providers, ",") {
			want[strings.TrimSpace(p)] = true
		}
		for k, e := range entries {
			if !want[e.LiteLLMProvider] {
				delete(entries, k)
			}
		}
	}

	// Filter by --models regex.
	if *models != "" {
		re, err := regexp.Compile(*models)
		if err != nil {
			slog.Error("import litellm: invalid --models regex", "err", err)
			os.Exit(1)
		}
		for k := range entries {
			if !re.MatchString(k) {
				delete(entries, k)
			}
		}
	}

	// Translate.
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{
		SourceVersion: version,
	})
	if err != nil {
		slog.Error("import litellm: translate failed", "err", err)
		os.Exit(1)
	}

	if !*apply {
		// Dry-run: print YAML to stdout.
		if err := printDryRun(result); err != nil {
			slog.Error("import litellm: dry-run print failed", "err", err)
			os.Exit(1)
		}
		// Print summary to stderr so it doesn't pollute stdout YAML.
		fmt.Fprintf(os.Stderr, "\nimport litellm (dry-run): %d models, %d providers, %d skipped (mode/provider).\nsource_version=%s\n",
			len(result.Models), len(result.Providers),
			result.SkippedMode+result.SkippedProvider, version)
		return
	}

	// Apply — needs PG.
	pgDSN := os.Getenv("RELAY_PG_DSN")
	if pgDSN == "" {
		slog.Error("import litellm: --apply requires RELAY_PG_DSN to be set")
		os.Exit(1)
	}

	st, err := storagemod.Open(ctx, pgDSN)
	if err != nil {
		slog.Error("import litellm: storage.Open failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	masterKeyBytes := parseMasterKeyEnv()
	pgStore, err := catalog.NewPGStore(st.Catalog, st, masterKeyBytes)
	if err != nil {
		slog.Error("import litellm: pgstore init failed", "err", err)
		os.Exit(1)
	}
	_ = pgStore // pgStore reloads snapshot on each write; we use st.Catalog directly for apply.

	applyMode := litellmimport.ApplyMode(*mode)
	ar, err := litellmimport.Apply(ctx, st.Catalog, st, result, litellmimport.ApplyOptions{
		Mode: applyMode,
	})
	if err != nil {
		slog.Error("import litellm: apply failed", "err", err)
		os.Exit(1)
	}

	slog.Info("import litellm: complete",
		"models_imported", ar.ModelsWritten,
		"models_skipped", ar.ModelsSkipped,
		"providers_ensured", ar.ProvidersWritten,
		"providers_skipped", ar.ProvidersSkipped,
		"skipped_mode", result.SkippedMode,
		"skipped_provider", result.SkippedProvider,
		"source_version", version,
		"mode", string(applyMode),
	)
	fmt.Printf("import litellm: %d models imported, %d providers ensured, %d models skipped (mode=embedding/audio/image), %d models skipped (provider not supported), 0 errors.\nsource_version=%s.\n",
		ar.ModelsWritten, ar.ProvidersWritten,
		result.SkippedMode, result.SkippedProvider, version)
}

func printDryRun(result *litellmimport.TranslateResult) error {
	// Sort providers and models alphabetically for consistent diffs.
	provs := make([]*catalog.Provider, len(result.Providers))
	copy(provs, result.Providers)
	sort.Slice(provs, func(i, j int) bool {
		return provs[i].Metadata.Name < provs[j].Metadata.Name
	})

	mods := make([]*catalog.Model, len(result.Models))
	copy(mods, result.Models)
	sort.Slice(mods, func(i, j int) bool {
		return mods[i].Metadata.Name < mods[j].Metadata.Name
	})

	for _, p := range provs {
		b, err := yaml.Marshal(p)
		if err != nil {
			return err
		}
		fmt.Printf("---\n%s", b)
	}
	for _, m := range mods {
		b, err := yaml.Marshal(m)
		if err != nil {
			return err
		}
		fmt.Printf("---\n%s", b)
	}
	return nil
}

func sourceFileOrURL(file, url string) string {
	if file != "" {
		return "file:" + file
	}
	return url
}

// parseMasterKeyEnv reads RELAY_MASTER_KEY from the environment (same logic as master_key.go).
// Returns nil if not set.
func parseMasterKeyEnv() []byte {
	v := os.Getenv("RELAY_MASTER_KEY")
	if v == "" {
		return nil
	}
	// Reuse the existing hex/base64 decode logic via the same env var that main uses.
	// Since we can't call the private decodeMasterKey, we reproduce a simple hex decode here.
	if len(v) == 64 {
		b := make([]byte, 32)
		for i := 0; i < 32; i++ {
			hi := hexVal(v[i*2])
			lo := hexVal(v[i*2+1])
			if hi < 0 || lo < 0 {
				return nil
			}
			b[i] = byte(hi<<4 | lo)
		}
		return b
	}
	return nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
