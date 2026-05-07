package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/storage"
)

//go:embed defaults/providers.yaml
var defaultProvidersYAML []byte

// seedDefaultProviders upserts the bundled default Providers if the catalog
// has none. No-op once the operator has created any provider — defaults
// never overwrite. Runs before the admin API comes online so the operator
// always sees a non-empty Providers list on first launch.
func seedDefaultProviders(ctx context.Context, store *catalog.PGStore, st *storage.Storage) error {
	if len(store.Providers()) > 0 {
		return nil
	}

	dec := yaml.NewDecoder(bytes.NewReader(defaultProvidersYAML))
	var providers []*catalog.Provider
	for {
		var p catalog.Provider
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("decode defaults/providers.yaml: %w", err)
		}
		if p.Kind != catalog.KindProvider || p.Metadata.Name == "" {
			continue
		}
		pp := p
		providers = append(providers, &pp)
	}

	if len(providers) == 0 {
		return nil
	}

	if err := st.WithTx(ctx, func(tx *storage.Storage) error {
		for _, p := range providers {
			if err := tx.Catalog.UpsertProvider(ctx, *p); err != nil {
				return fmt.Errorf("upsert default provider %q: %w", p.Metadata.Name, err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("seed defaults tx: %w", err)
	}

	if err := store.Reload(ctx); err != nil {
		return fmt.Errorf("reload after default seed: %w", err)
	}

	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Metadata.Name)
	}
	slog.Info("seeded default providers", "providers", names)
	return nil
}
