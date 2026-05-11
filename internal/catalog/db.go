package catalog

import "context"

// CatalogDB is the storage interface that PGStore uses for data access.
// *storage.Storage satisfies this interface (via its Catalog field methods
// promoted to the top-level if needed, or by wrapping).
// Defining the interface here breaks the import cycle: catalog defines what it
// needs; storage satisfies it. catalog does not import storage.
type CatalogDB interface {
	// UpsertProvider inserts or updates a provider.
	UpsertProvider(ctx context.Context, p Provider) error
	// ListProviders returns all providers.
	ListProviders(ctx context.Context) ([]Provider, error)
	// DeleteProvider removes a provider.
	DeleteProvider(ctx context.Context, name string) error

	// UpsertPolicy inserts or updates a policy.
	UpsertPolicy(ctx context.Context, p Policy) error
	// ListPolicies returns all policies.
	ListPolicies(ctx context.Context) ([]Policy, error)
	// DeletePolicy removes a policy.
	DeletePolicy(ctx context.Context, name string) error

	// ListSecretRows returns raw secret rows (with encrypted bytes) for snapshot loading.
	ListSecretRows(ctx context.Context) ([]SecretRow, error)
	// UpsertSecretEnv inserts or updates a secret in env-ref mode.
	UpsertSecretEnv(ctx context.Context, name, envVar, provider string, meta Metadata) error
	// UpsertSecretStored inserts or updates a secret in stored (encrypted) mode.
	// The ciphertext and nonce must already be computed by the catalog layer.
	UpsertSecretStored(ctx context.Context, name, provider string, meta Metadata, ciphertext, nonce []byte) error
	// UpdateSecretEnv changes an existing secret to env-ref mode.
	UpdateSecretEnv(ctx context.Context, name, envVar string) error
	// UpdateSecretStored rotates the ciphertext for a stored-mode secret.
	UpdateSecretStored(ctx context.Context, name string, ciphertext, nonce []byte) error
	// DeleteSecret removes a secret.
	DeleteSecret(ctx context.Context, name string) error
	// UpsertSecretRaw writes a secret using the legacy upsert query (YAML seed path).
	UpsertSecretRaw(ctx context.Context, name string, meta Metadata, spec SecretSpec) error

	// UpsertModel inserts or updates a model.
	UpsertModel(ctx context.Context, m Model) error
	// ListModels returns all models.
	ListModels(ctx context.Context) ([]Model, error)
	// DeleteModel removes a model.
	DeleteModel(ctx context.Context, name string) error

	// UpsertRoute inserts or updates a route.
	UpsertRoute(ctx context.Context, r Route) error
	// ListRoutes returns all routes.
	ListRoutes(ctx context.Context) ([]Route, error)
	// DeleteRoute removes a route.
	DeleteRoute(ctx context.Context, name string) error

	// UpsertRateLimit inserts or updates a rate limit.
	UpsertRateLimit(ctx context.Context, rl RateLimit) error
	// ListRateLimits returns all rate limits.
	ListRateLimits(ctx context.Context) ([]RateLimit, error)
	// DeleteRateLimit removes a rate limit.
	DeleteRateLimit(ctx context.Context, name string) error

	// IsEmpty returns true when all catalog tables have zero rows.
	IsEmpty(ctx context.Context) (bool, error)

	// UpsertRelayKey inserts or updates a relay key.
	UpsertRelayKey(ctx context.Context, k RelayKey) error
	// ListRelayKeys returns all relay keys.
	ListRelayKeys(ctx context.Context) ([]RelayKey, error)
	// DeleteRelayKey removes a relay key.
	DeleteRelayKey(ctx context.Context, name string) error
}

// TxRunner runs fn inside a transaction, committing on nil error.
// *storage.Storage satisfies this interface.
type TxRunner interface {
	WithTxCatalog(ctx context.Context, fn func(db CatalogDB) error) error
}

// SecretRow is the raw secret data returned from the database.
// It carries the encrypted bytes and value_kind so the catalog layer can
// decrypt/resolve without storage needing the master key.
type SecretRow struct {
	Name            string
	Metadata        Metadata
	Spec            SecretSpec
	ValueKind       string
	ValueFromEnv    string
	ValueFromEnvSet bool
	ValueCiphertext []byte
	ValueNonce      []byte
}
