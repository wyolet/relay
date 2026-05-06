# Storage Layer — Conventions & Rules

> Status: target architecture. Live code may not yet conform. New PRs MUST.

## Purpose

`internal/storage` is Wyolet Relay's **data-access service layer**. It owns the entire surface that talks to the primary database (Postgres). Every other package consumes storage through typed, domain-shaped method calls; nobody else sees `pgx`, sqlc-generated types, or SQL.

The split is between two tiers:

- **`internal/storage`** — data-access tier. Where bytes live. Owns pgx, sqlc, SQL, migrations, transactions, error translation. Has no opinion on what the data *means*.
- **`internal/catalog`** (and future siblings: `internal/audit`, `internal/batch`, ...) — domain tier. Owns types, interfaces, validation, snapshots, business rules. Calls storage; never touches pgx.

This is closer to a Python/Django service layer than a Java repository pattern: storage is **one struct grouped by domain area**, not a galaxy of `IXxxRepository` interfaces.

## Layout

```
internal/
  storage/
    storage.go         # *Storage struct, Open(ctx, dsn) (*Storage, error), Close()
    pool.go            # pgxpool wiring, health checks, stats
    tx.go              # WithTx helper — the only way domain code asks for atomicity
    migrate.go         # runs golang-migrate on Open
    errors.go          # translation from pgx errors to domain errors
    catalog.go         # Catalog repo: typed methods for catalog domain
    secrets.go         # Secret writes (env-ref vs stored ciphertext columns)
    audit.go           # (future) audit log writes
    batch.go           # (future) batch job state
    queries/           # sqlc YAML + .sql sources
      queries.sql
      sqlc.yaml
    gen/               # sqlc-generated code — internal-only, never imported elsewhere
      models.go
      queries.sql.go
  catalog/             # domain types + Store interface + impls (one of which uses *storage.Storage)
    types.go
    interface.go
    snapshot.go
    validate.go
    patch.go
    pgstore.go         # implements catalog.Store via *storage.Storage; no pgx imports
    yaml.go            # dev/local backend (no storage dep)
    mem.go             # test backend (no storage dep)
```

`migrations/postgres/*.sql` stays where it is — those are SQL artifacts, not Go code.

## Hard Rules

These are non-negotiable. Reviewers should reject PRs that break them.

### 1. No `pgx` types in any exported signature outside `internal/storage`.

Never `pgx.Tx`, `pgxpool.Pool`, `pgx.Rows`, `pgx.Conn`, `pgx.ErrNoRows`, `*pgconn.PgError`. Translate at the boundary.

### 2. No SQL strings outside `internal/storage`.

`SELECT`, `INSERT`, `UPDATE`, `DELETE`, `CREATE`, `ALTER` literals appear only in `internal/storage` (and the migrations directory). sqlc-generated SQL is fine because it lives there.

### 3. No sqlc-generated types in exported signatures.

The `internal/storage/gen` package is for storage's eyes only. Storage methods accept and return **domain types** (defined in `internal/catalog` and similar). Conversion happens at the boundary inside storage.

### 4. Transactions are storage-managed.

Domain code never holds a `*pgx.Tx`. Atomicity is expressed two ways:

- **Single method:** when an operation always needs to be atomic, expose it as one storage method (`s.Catalog.UpsertProviderWithSecrets(...)`).
- **WithTx callback:** when the domain layer needs to compose multiple storage calls atomically:

  ```go
  err := s.WithTx(ctx, func(tx *storage.Storage) error {
      if err := tx.Catalog.UpsertProvider(ctx, p); err != nil { return err }
      if err := tx.Catalog.UpsertPool(ctx, pool); err != nil { return err }
      return nil
  })
  ```

  The `tx` parameter is the same `*Storage` shape — same methods — just bound to a transaction internally. Domain code never sees the underlying `pgx.Tx`.

### 5. Storage returns domain errors.

Never propagate `pgx.ErrNoRows`. Translate to `catalog.ErrNotFound`. Never propagate `*pgconn.PgError{Code: "23505"}`. Translate to `catalog.ErrConflict` (or domain-specific equivalent). Error translation is centralized in `internal/storage/errors.go`.

Domain packages export their own sentinel errors. Storage knows about those domain errors (storage imports domain). Domain knows nothing about pgx.

### 6. No "executor" abstraction.

Resist the temptation to expose a `storage.Executor` interface that abstracts over `*pgx.Tx` and `*pgxpool.Pool` so a single domain function can run "in or out of transaction." That dual-mode pattern leaks pgx's transaction model into the domain layer's mental model.

If you need that flexibility, use `WithTx` as in rule 4 — the same `*Storage` shape works inside and outside a transaction without exposing the difference.

### 7. Group storage by domain area, not by entity.

```go
type Storage struct {
    Catalog *catalogRepo    // grouped: Provider, Pool, Secret, Model, Route, RateLimit
    Audit   *auditRepo      // grouped: audit log writes
    Batch   *batchRepo      // grouped: batch jobs (future)

    pool    *pgxpool.Pool   // unexported
}
```

Read sites become readable: `s.Catalog.ListSecrets(ctx)` is clearer than `s.ListSecrets(ctx)` (ambiguous: what kind of secrets?). Don't create one repo struct per entity (`ProviderRepo`, `PoolRepo`, `SecretRepo`, ...) — that's repository-pattern bloat.

## Method-Signature Style

Storage methods accept and return domain types:

```go
// internal/storage/catalog.go
func (r *catalogRepo) UpsertProvider(ctx context.Context, p catalog.Provider) error
func (r *catalogRepo) GetProvider(ctx context.Context, name string) (catalog.Provider, error)
func (r *catalogRepo) ListProviders(ctx context.Context) ([]catalog.Provider, error)
func (r *catalogRepo) DeleteProvider(ctx context.Context, name string) error
```

Implementations convert sqlc rows ↔ domain types inline. The conversion code is dull but it's the firewall.

## Domain Errors

Each domain package owns its sentinel errors:

```go
// internal/catalog/errors.go
var (
    ErrNotFound    = errors.New("catalog: not found")
    ErrConflict    = errors.New("catalog: name conflict")
    ErrInvalidSpec = errors.New("catalog: invalid spec")
)
```

Storage imports the domain package's errors and returns them after translation:

```go
// internal/storage/errors.go
func translateCatalogErr(err error) error {
    if errors.Is(err, pgx.ErrNoRows)         { return catalog.ErrNotFound }
    var pgErr *pgconn.PgError
    if errors.As(err, &pgErr) {
        switch pgErr.Code {
        case "23505": return catalog.ErrConflict   // unique violation
        case "23503": return catalog.ErrInvalidSpec // FK violation
        }
    }
    return err
}
```

Domain code uses `errors.Is(err, catalog.ErrNotFound)` and never imports pgx.

## Encryption & Secrets

Secret encryption decisions live in `internal/catalog` (the *policy* — what to encrypt, when, with which key). The crypto primitives live in `pkg/crypto`. Storage handles the *bytes* — accepting `valueCiphertext`, `valueNonce`, `valueKind` as already-encoded fields on the domain type and writing them to the right columns.

Storage does NOT call `crypto.Encrypt`. Storage does NOT decide whether a secret is env-ref or stored. By the time a secret reaches `s.Catalog.UpsertSecret`, that decision has been made.

This keeps storage stateless about encryption. The master key never enters `internal/storage`.

## Migrations

Migrations live in `migrations/postgres/`. They are run automatically by `storage.Open()` via golang-migrate. Migrations are SQL-only — no Go-side migration logic. If a migration needs to backfill data through Go code, it's a separate one-shot tool in `cmd/`, not part of `storage.Open()`.

Migration files are append-only and versioned (`000001_init.up.sql`, etc.). Never edit a merged migration; write a new one.

## Testing

- **Unit tests for `internal/catalog`** mock `*storage.Storage` via a small interface declared in catalog (consumer-side, narrow — only the methods catalog uses). No real Postgres needed for catalog logic tests.
- **Integration tests for `internal/storage`** use testcontainers-postgres. They verify the SQL, the conversions, and the error translation.
- **End-to-end tests in `cmd/relay`** wire real storage and real catalog together with testcontainers.

## Grep Tests (the boundary's smoke alarm)

Run these locally before opening a PR. CI should run them too.

```sh
# No pgx leakage outside storage
rg 'pgx\.|pgxpool\.|pgconn\.' --type go -g '!internal/storage/**'

# No raw SQL outside storage and migrations
rg '\b(SELECT|INSERT|UPDATE|DELETE)\b' --type go -g '*.go' -g '!internal/storage/**'

# sqlc-generated types should never appear outside storage
rg 'storage/gen' --type go -g '!internal/storage/**'
```

All three must return zero hits. If they don't, the boundary is leaking and the PR needs to translate before it merges.

## Adding a New Storage Area — Checklist

When you add a new domain (e.g. `internal/audit` or `internal/batch`):

1. Define domain types and sentinel errors in the domain package (`internal/<name>`).
2. Create `internal/storage/<name>.go` with the new repo struct (`auditRepo`, `batchRepo`).
3. Add the repo as a field on `*Storage` in `storage.go`.
4. Wire the repo's constructor in `Open()`.
5. Add error translations to `internal/storage/errors.go` if needed.
6. Add testcontainer-backed tests in `internal/storage/<name>_test.go`.
7. Run the grep tests; confirm zero leakage.

## Anti-Patterns

- **Returning `pgx.Rows`** from a storage method so the caller can iterate. Iterate inside storage; return a slice or stream a channel.
- **Accepting a `Querier` interface** that abstracts pool vs tx. Use `WithTx` instead.
- **Smuggling SQL through string concatenation** in domain code "just for one query." If it's worth a query, it's worth a storage method.
- **One repo struct per entity** (`ProviderRepo`, `PoolRepo`, `SecretRepo`, ...). Group by area.
- **Generic CRUD**: `Repo[T any]`. Resist. Each domain has its own shape; the typed methods earn their keep.
- **Calling `crypto.Encrypt` inside storage.** Encryption policy lives in domain.
- **Returning sqlc types**, even "just internally." If it's exported from the storage package, it must be a domain type.

## Non-Goals for `internal/storage`

- **No automatic retries.** Caller decides retry semantics. Storage just propagates errors.
- **No connection-pool tuning knobs exposed.** Pool sizing is configured at `Open()` time and is an implementation detail.
- **No multi-database support.** One Postgres instance per Relay deployment. If we ever need read replicas or sharding, that's a future redesign — don't pre-empt it with abstractions today.
- **No ORM-like dirty tracking, lazy loading, or unit-of-work pattern.** Methods are explicit; transactions are explicit via `WithTx`.

## Why This Layering Exists

The discipline isn't about being able to swap Postgres for something else (we're not planning to). It's about:

1. **Reviewability.** Every PR that touches storage logic touches `internal/storage`. SQL changes live in one tree.
2. **Testability.** Domain logic tests don't need a database.
3. **AI-navigability.** Claude (or any future contributor) reading the catalog package isn't drowning in pgx noise. The intent of the code is visible.
4. **Schema as a single surface area.** When the schema changes, every dependent query is in `internal/storage/queries/` and `internal/storage/*.go`. No archaeology required.

If a rule above ever feels like ceremony, ask whether deleting it would create one of those four problems. If yes, keep the rule.
