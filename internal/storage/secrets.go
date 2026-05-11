package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/storage/gen"
)

// UpsertSecretEnv inserts or updates a secret in env-ref mode.
// Encryption decisions live in catalog — storage just writes the columns.
// meta.ID is the immutable PK; meta.Name is the slug.
func (r *catalogRepo) UpsertSecretEnv(ctx context.Context, envVar, provider string, meta catalog.Metadata) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretEnv %q: marshal meta: %w", meta.Name, err)
	}
	specJSON, err := json.Marshal(catalog.SecretSpec{
		Provider:  provider,
		ValueFrom: &catalog.SecretValueFrom{Env: envVar},
	})
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretEnv %q: marshal spec: %w", meta.Name, err)
	}
	_, err = gen.New(r.db).InsertSecretEnv(ctx, gen.InsertSecretEnvParams{
		ID:           meta.ID,
		Name:         meta.Name,
		DisplayName:  meta.DisplayName,
		ValueFromEnv: pgtype.Text{String: envVar, Valid: true},
		Metadata:     metaJSON,
		Spec:         specJSON,
	})
	if err := translateCatalogErr(err); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// UpsertSecretStored inserts or updates a secret in stored (encrypted) mode.
// The ciphertext and nonce must already be computed by the catalog layer.
func (r *catalogRepo) UpsertSecretStored(ctx context.Context, provider string, meta catalog.Metadata, ciphertext, nonce []byte) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretStored %q: marshal meta: %w", meta.Name, err)
	}
	specJSON, err := json.Marshal(catalog.SecretSpec{Provider: provider})
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretStored %q: marshal spec: %w", meta.Name, err)
	}
	_, err = gen.New(r.db).InsertSecretStored(ctx, gen.InsertSecretStoredParams{
		ID:              meta.ID,
		Name:            meta.Name,
		DisplayName:     meta.DisplayName,
		ValueCiphertext: ciphertext,
		ValueNonce:      nonce,
		Metadata:        metaJSON,
		Spec:            specJSON,
	})
	if err := translateCatalogErr(err); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// UpdateSecretEnv changes an existing secret to env-ref mode (id-routed).
func (r *catalogRepo) UpdateSecretEnv(ctx context.Context, id, envVar string) error {
	_, err := gen.New(r.db).UpdateSecretEnv(ctx, gen.UpdateSecretEnvParams{
		ID:           id,
		ValueFromEnv: pgtype.Text{String: envVar, Valid: true},
	})
	if err := translateCatalogErr(err); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// UpdateSecretStored rotates the ciphertext for a stored-mode secret (id-routed).
func (r *catalogRepo) UpdateSecretStored(ctx context.Context, id string, ciphertext, nonce []byte) error {
	_, err := gen.New(r.db).UpdateSecretStored(ctx, gen.UpdateSecretStoredParams{
		ID:              id,
		ValueCiphertext: ciphertext,
		ValueNonce:      nonce,
	})
	if err := translateCatalogErr(err); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// DeleteSecret removes a secret by id.
func (r *catalogRepo) DeleteSecret(ctx context.Context, id string) error {
	if err := translateCatalogErr(gen.New(r.db).DeleteSecret(ctx, id)); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}
