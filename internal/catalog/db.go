package catalog

import "context"

// CatalogDB is the storage interface that PGStore uses for data access.
// *storage.Storage satisfies this interface (via its Catalog field methods
// promoted to the top-level if needed, or by wrapping).
// Defining the interface here breaks the import cycle: catalog defines what it
// needs; storage satisfies it. catalog does not import storage.
type CatalogDB interface {
	// All Upsert* take a fully-formed resource. The caller is responsible for
	// stamping Metadata.ID (UUIDv7) on first create. All Delete* are id-routed.

	UpsertProvider(ctx context.Context, p Provider) error
	ListProviders(ctx context.Context) ([]Provider, error)
	DeleteProvider(ctx context.Context, id string) error

	UpsertPolicy(ctx context.Context, p Policy) error
	ListPolicies(ctx context.Context) ([]Policy, error)
	DeletePolicy(ctx context.Context, id string) error

	// ListSecretRows returns raw secret rows (with encrypted bytes) for snapshot loading.
	ListSecretRows(ctx context.Context) ([]SecretRow, error)
	UpsertSecretEnv(ctx context.Context, envVar, provider string, meta Metadata) error
	UpsertSecretStored(ctx context.Context, provider string, meta Metadata, ciphertext, nonce []byte) error
	UpdateSecretEnv(ctx context.Context, id, envVar string) error
	UpdateSecretStored(ctx context.Context, id string, ciphertext, nonce []byte) error
	DeleteSecret(ctx context.Context, id string) error
	// UpsertSecretRaw writes a secret using the legacy upsert query (YAML seed path).
	UpsertSecretRaw(ctx context.Context, meta Metadata, spec SecretSpec) error

	UpsertModel(ctx context.Context, m Model) error
	ListModels(ctx context.Context) ([]Model, error)
	DeleteModel(ctx context.Context, id string) error

	UpsertRoute(ctx context.Context, r Route) error
	ListRoutes(ctx context.Context) ([]Route, error)
	DeleteRoute(ctx context.Context, id string) error

	UpsertRateLimit(ctx context.Context, rl RateLimit) error
	ListRateLimits(ctx context.Context) ([]RateLimit, error)
	DeleteRateLimit(ctx context.Context, id string) error

	// IsEmpty returns true when all catalog tables have zero rows.
	IsEmpty(ctx context.Context) (bool, error)

	UpsertRelayKey(ctx context.Context, k RelayKey) error
	ListRelayKeys(ctx context.Context) ([]RelayKey, error)
	DeleteRelayKey(ctx context.Context, id string) error

	// GetPassthrough returns the singleton row, or (nil, nil) when unset.
	GetPassthrough(ctx context.Context) (*Passthrough, error)
	// SetPassthrough upserts the singleton row.
	SetPassthrough(ctx context.Context, p Passthrough) error
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
	ID              string
	Name            string
	DisplayName     string
	Metadata        Metadata
	Spec            SecretSpec
	ValueKind       string
	ValueFromEnv    string
	ValueFromEnvSet bool
	ValueCiphertext []byte
	ValueNonce      []byte
}
