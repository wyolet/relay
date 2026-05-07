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
func (r *catalogRepo) UpsertSecretEnv(ctx context.Context, name, envVar, provider string, meta catalog.Metadata) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretEnv %q: marshal meta: %w", name, err)
	}
	specJSON, err := json.Marshal(catalog.SecretSpec{
		Provider:  provider,
		ValueFrom: &catalog.SecretValueFrom{Env: envVar},
	})
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretEnv %q: marshal spec: %w", name, err)
	}
	_, err = gen.New(r.db).InsertSecretEnv(ctx, gen.InsertSecretEnvParams{
		Name:         name,
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
func (r *catalogRepo) UpsertSecretStored(ctx context.Context, name, provider string, meta catalog.Metadata, ciphertext, nonce []byte) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretStored %q: marshal meta: %w", name, err)
	}
	specJSON, err := json.Marshal(catalog.SecretSpec{Provider: provider})
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretStored %q: marshal spec: %w", name, err)
	}
	_, err = gen.New(r.db).InsertSecretStored(ctx, gen.InsertSecretStoredParams{
		Name:            name,
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

// UpdateSecretEnv changes an existing secret to env-ref mode.
func (r *catalogRepo) UpdateSecretEnv(ctx context.Context, name, envVar string) error {
	_, err := gen.New(r.db).UpdateSecretEnv(ctx, gen.UpdateSecretEnvParams{
		Name:         name,
		ValueFromEnv: pgtype.Text{String: envVar, Valid: true},
	})
	if err := translateCatalogErr(err); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// UpdateSecretStored rotates the ciphertext for a stored-mode secret.
// The new ciphertext and nonce must already be computed by the catalog layer.
func (r *catalogRepo) UpdateSecretStored(ctx context.Context, name string, ciphertext, nonce []byte) error {
	_, err := gen.New(r.db).UpdateSecretStored(ctx, gen.UpdateSecretStoredParams{
		Name:            name,
		ValueCiphertext: ciphertext,
		ValueNonce:      nonce,
	})
	if err := translateCatalogErr(err); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// DeleteSecret removes a secret by name.
func (r *catalogRepo) DeleteSecret(ctx context.Context, name string) error {
	if err := translateCatalogErr(gen.New(r.db).DeleteSecret(ctx, name)); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}
