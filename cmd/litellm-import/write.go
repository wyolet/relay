package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/manifest"
)

// WriteResult summarises what happened during a write-to-disk pass.
type WriteResult struct {
	ProvidersWritten int
	HostsWritten     int
	ModelsWritten    int
	PricingsWritten  int
	Errors           int
}

// SanitizeFilename replaces unsafe filename characters (':', '/') with '_'.
func SanitizeFilename(name string) string {
	return strings.NewReplacer(":", "_", "/", "_").Replace(name)
}

// WriteToDisk writes translated DTOs as YAML files under outDir, using the
// catalog-repo layout consumed by wyolet/relay-catalog:
//
//	<outDir>/hosts/<host>/host.yaml
//	<outDir>/hosts/<host>/pricing/<model>.yaml
//	<outDir>/providers/<provider>/provider.yaml
//	<outDir>/providers/<provider>/models/<model>.yaml
//
// Existing files are always overwritten. Pricing nests under the owning
// host (Owner.ID on PricingDTO carries the host name). Hand-curated files
// the importer doesn't emit (policies/, secrets/, ...) are left untouched
// when outDir contains them.
func WriteToDisk(outDir string, result *TranslateResult) (*WriteResult, error) {
	wr := &WriteResult{}

	for _, h := range result.Hosts {
		path := filepath.Join(outDir, "hosts", h.Metadata.Name, "host.yaml")
		if err := writeYAML(path, h); err != nil {
			slog.Error("litellm-import: write host failed", "path", path, "err", err)
			wr.Errors++
			continue
		}
		slog.Info("litellm-import: wrote " + path)
		wr.HostsWritten++
	}

	for _, p := range result.Providers {
		path := filepath.Join(outDir, "providers", p.Metadata.Name, "provider.yaml")
		if err := writeYAML(path, p); err != nil {
			slog.Error("litellm-import: write provider failed", "path", path, "err", err)
			wr.Errors++
			continue
		}
		slog.Info("litellm-import: wrote " + path)
		wr.ProvidersWritten++
	}

	for _, m := range result.Models {
		// buildModel populates Owner.Name (Owner.ID stays empty for the
		// pre-resolution wire form). Fall back to Owner.ID for robustness.
		providerName := m.Metadata.Owner.Name
		if providerName == "" {
			providerName = m.Metadata.Owner.ID
		}
		filename := SanitizeFilename(m.Metadata.Name) + ".yaml"
		path := filepath.Join(outDir, "providers", providerName, "models", filename)
		if err := writeYAML(path, m); err != nil {
			slog.Error("litellm-import: write model failed", "path", path, "err", err)
			wr.Errors++
			continue
		}
		slog.Info("litellm-import: wrote " + path)
		wr.ModelsWritten++
	}

	for _, p := range result.Pricings {
		hostName := p.Metadata.Owner.ID
		// Strip the "<host>-" prefix added in buildPricing for a cleaner
		// per-host filename (the host directory already carries the host).
		base := strings.TrimPrefix(p.Metadata.Name, hostName+"-")
		if base == p.Metadata.Name {
			base = p.Metadata.Name
		}
		filename := SanitizeFilename(base) + ".yaml"
		path := filepath.Join(outDir, "hosts", hostName, "pricing", filename)
		if err := writeYAML(path, p); err != nil {
			slog.Error("litellm-import: write pricing failed", "path", path, "err", err)
			wr.Errors++
			continue
		}
		slog.Info("litellm-import: wrote " + path)
		wr.PricingsWritten++
	}

	return wr, nil
}

func writeYAML(path string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return os.WriteFile(path, b, 0o644)
}

// WriteToStdout emits all hosts, providers, then models as YAML documents to stdout.
func WriteToStdout(result *TranslateResult) error {
	emit := func(v any) error {
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		fmt.Printf("---\n%s", b)
		return nil
	}
	for _, h := range result.Hosts {
		if err := emit(h); err != nil {
			return fmt.Errorf("marshal host %s: %w", h.Metadata.Name, err)
		}
	}
	for _, p := range result.Providers {
		if err := emit(p); err != nil {
			return fmt.Errorf("marshal provider %s: %w", p.Metadata.Name, err)
		}
	}
	for _, m := range result.Models {
		if err := emit(m); err != nil {
			return fmt.Errorf("marshal model %s: %w", m.Metadata.Name, err)
		}
	}
	for _, p := range result.Pricings {
		if err := emit(p); err != nil {
			return fmt.Errorf("marshal pricing %s: %w", p.Metadata.Name, err)
		}
	}
	return nil
}

// Ensure manifest types compile.
var _ = manifest.ProviderDTO{}
