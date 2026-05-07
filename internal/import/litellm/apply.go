package litellm

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wyolet/relay/internal/catalog"
)

// ApplyMode controls how existing entries are handled during import.
type ApplyMode string

const (
	// ModeUpsert overwrites any existing entry with the new data (default).
	ModeUpsert ApplyMode = "upsert"
	// ModeSkipExisting leaves entries that already exist untouched.
	ModeSkipExisting ApplyMode = "skip-existing"
	// ModeOverwrite is identical to ModeUpsert but uses a different log message.
	ModeOverwrite ApplyMode = "overwrite"
)

// ApplyOptions configures the apply pass.
type ApplyOptions struct {
	Mode ApplyMode
}

// ApplyResult summarises what happened.
type ApplyResult struct {
	ProvidersWritten int
	ProvidersSkipped int
	ModelsWritten    int
	ModelsSkipped    int
}

// Apply writes the translated entities to storage via the CatalogDB interface.
// The entire write is wrapped in a single transaction via txRunner.
// For dry-run: do not call Apply — marshal the result to YAML and print instead.
func Apply(ctx context.Context, db catalog.CatalogDB, txRunner catalog.TxRunner, result *TranslateResult, opts ApplyOptions) (*ApplyResult, error) {
	mode := opts.Mode
	if mode == "" {
		mode = ModeUpsert
	}

	// When mode=skip-existing, fetch the current snapshot to know what exists.
	existingProviders := map[string]bool{}
	existingModels := map[string]bool{}
	if mode == ModeSkipExisting {
		provs, err := db.ListProviders(ctx)
		if err != nil {
			return nil, fmt.Errorf("apply: list providers: %w", err)
		}
		for _, p := range provs {
			existingProviders[p.Metadata.Name] = true
		}
		models, err := db.ListModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("apply: list models: %w", err)
		}
		for _, m := range models {
			existingModels[m.Metadata.Name] = true
		}
	}

	ar := &ApplyResult{}

	err := txRunner.WithTxCatalog(ctx, func(txDB catalog.CatalogDB) error {
		for _, p := range result.Providers {
			name := p.Metadata.Name
			if mode == ModeSkipExisting && existingProviders[name] {
				slog.Info("import litellm: provider exists, skipping", "provider", name, "mode", string(mode))
				ar.ProvidersSkipped++
				continue
			}
			if err := txDB.UpsertProvider(ctx, *p); err != nil {
				return fmt.Errorf("apply: UpsertProvider %q: %w", name, err)
			}
			slog.Info("import litellm: provider written", "provider", name, "mode", string(mode))
			ar.ProvidersWritten++
		}

		for _, m := range result.Models {
			name := m.Metadata.Name
			if mode == ModeSkipExisting && existingModels[name] {
				slog.Info("import litellm: model exists, skipping", "model", name, "mode", string(mode))
				ar.ModelsSkipped++
				continue
			}
			if err := txDB.UpsertModel(ctx, *m); err != nil {
				return fmt.Errorf("apply: UpsertModel %q: %w", name, err)
			}
			slog.Info("import litellm: model written", "model", name, "mode", string(mode))
			ar.ModelsWritten++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ar, nil
}
