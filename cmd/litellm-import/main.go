package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

func main() {
	fs := flag.NewFlagSet("litellm-import", flag.ExitOnError)
	out := fs.String("out", "config", `Output directory for YAML files. Use "-" to write to stdout.`)
	providers := fs.String("providers", "", "Comma-separated litellm_provider values to include (default: all)")
	models := fs.String("models", "", "Regex to filter model names (default: all)")
	sourceURL := fs.String("source-url", DefaultLiteLLMURL, "Override the LiteLLM JSON URL")
	sourceFile := fs.String("source-file", "", "Read JSON from a local file instead of fetching")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	ctx := context.Background()

	src := *sourceURL
	if *sourceFile != "" {
		src = "file:" + *sourceFile
	}
	slog.Info("litellm-import: fetching", "source", src)

	entries, version, err := Fetch(ctx, *sourceURL, *sourceFile)
	if err != nil {
		slog.Error("litellm-import: fetch failed", "err", err)
		os.Exit(1)
	}
	slog.Info("litellm-import: fetched", "entries", len(entries), "version", version)

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
			slog.Error("litellm-import: invalid --models regex", "err", err)
			os.Exit(1)
		}
		for k := range entries {
			if !re.MatchString(k) {
				delete(entries, k)
			}
		}
	}

	result, err := Translate(entries, version)
	if err != nil {
		slog.Error("litellm-import: translate failed", "err", err)
		os.Exit(1)
	}

	if *out == "-" {
		if err := WriteToStdout(result); err != nil {
			slog.Error("litellm-import: stdout write failed", "err", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "litellm-import (stdout): %d hosts, %d providers, %d models, %d skipped.\nsource_version=%s\n",
			len(result.Hosts), len(result.Providers), len(result.Models),
			result.SkippedMode+result.SkippedProvider, version)
		return
	}

	wr, err := WriteToDisk(*out, result)
	if err != nil {
		slog.Error("litellm-import: write-to-disk failed", "err", err)
		os.Exit(1)
	}
	slog.Info("litellm-import: complete",
		"hosts_written", wr.HostsWritten,
		"providers_written", wr.ProvidersWritten,
		"models_written", wr.ModelsWritten,
		"pricings_written", wr.PricingsWritten,
		"skipped_mode", result.SkippedMode,
		"skipped_provider", result.SkippedProvider,
		"errors", wr.Errors,
		"out", *out,
		"source_version", version,
	)
	fmt.Fprintf(os.Stderr, "litellm-import: %d hosts, %d providers, %d models, %d pricings written, %d skipped (mode/provider), %d errors.\nsource_version=%s\n",
		wr.HostsWritten, wr.ProvidersWritten, wr.ModelsWritten, wr.PricingsWritten,
		result.SkippedMode+result.SkippedProvider, wr.Errors, version)
}
