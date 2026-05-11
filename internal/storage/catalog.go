package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/storage/gen"
)

// catalogRepo holds all data-access methods for the catalog domain.
// All methods accept and return domain types from internal/catalog.
// sqlc-generated types are an implementation detail; they never leave this package.
//
// All Upsert* require Metadata.ID to be set (UUIDv7, stamped by domain code on
// create). All Delete* are id-routed.
type catalogRepo struct {
	db gen.DBTX
}

// ── Provider ──────────────────────────────────────────────────────────────────

func (r *catalogRepo) UpsertProvider(ctx context.Context, p catalog.Provider) error {
	meta, spec, err := marshalMetaSpec(p.Metadata, p.Spec)
	if err != nil {
		return fmt.Errorf("storage: UpsertProvider %q: %w", p.Metadata.Name, err)
	}
	err = gen.New(r.db).UpsertProvider(ctx, gen.UpsertProviderParams{
		ID:          p.Metadata.ID,
		Name:        p.Metadata.Name,
		DisplayName: p.Metadata.DisplayName,
		Metadata:    meta,
		Spec:        spec,
	})
	if err := translateCatalogErr(err); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

func (r *catalogRepo) ListProviders(ctx context.Context) ([]catalog.Provider, error) {
	rows, err := gen.New(r.db).ListProviders(ctx)
	if err != nil {
		return nil, translateCatalogErr(err)
	}
	out := make([]catalog.Provider, 0, len(rows))
	for _, row := range rows {
		var meta catalog.Metadata
		var spec catalog.ProviderSpec
		if err := unmarshalJSON2(row.Metadata, &meta, row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("storage: provider %q: %w", row.Name, err)
		}
		reconcileMeta(&meta, row.ID, row.Name, row.DisplayName)
		out = append(out, catalog.Provider{
			APIVersion: catalog.APIVersion,
			Kind:       catalog.KindProvider,
			Metadata:   meta,
			Spec:       spec,
		})
	}
	return out, nil
}

func (r *catalogRepo) DeleteProvider(ctx context.Context, id string) error {
	if err := translateCatalogErr(gen.New(r.db).DeleteProvider(ctx, id)); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// ── Policy ──────────────────────────────────────────────────────────────────────

func (r *catalogRepo) UpsertPolicy(ctx context.Context, p catalog.Policy) error {
	meta, spec, err := marshalMetaSpec(p.Metadata, p.Spec)
	if err != nil {
		return fmt.Errorf("storage: UpsertPolicy %q: %w", p.Metadata.Name, err)
	}
	if err := translateCatalogErr(gen.New(r.db).UpsertPolicy(ctx, gen.UpsertPolicyParams{
		ID:          p.Metadata.ID,
		Name:        p.Metadata.Name,
		DisplayName: p.Metadata.DisplayName,
		Metadata:    meta,
		Spec:        spec,
	})); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

func (r *catalogRepo) ListPolicies(ctx context.Context) ([]catalog.Policy, error) {
	rows, err := gen.New(r.db).ListPolicies(ctx)
	if err != nil {
		return nil, translateCatalogErr(err)
	}
	out := make([]catalog.Policy, 0, len(rows))
	for _, row := range rows {
		var meta catalog.Metadata
		var spec catalog.PolicySpec
		if err := unmarshalJSON2(row.Metadata, &meta, row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("storage: policy %q: %w", row.Name, err)
		}
		reconcileMeta(&meta, row.ID, row.Name, row.DisplayName)
		out = append(out, catalog.Policy{
			APIVersion: catalog.APIVersion,
			Kind:       catalog.KindPolicy,
			Metadata:   meta,
			Spec:       spec,
		})
	}
	return out, nil
}

func (r *catalogRepo) DeletePolicy(ctx context.Context, id string) error {
	if err := translateCatalogErr(gen.New(r.db).DeletePolicy(ctx, id)); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// ── Secret reads ──────────────────────────────────────────────────────────────

func (r *catalogRepo) ListSecretRows(ctx context.Context) ([]catalog.SecretRow, error) {
	rows, err := gen.New(r.db).ListSecrets(ctx)
	if err != nil {
		return nil, translateCatalogErr(err)
	}
	out := make([]catalog.SecretRow, 0, len(rows))
	for _, row := range rows {
		var meta catalog.Metadata
		var spec catalog.SecretSpec
		if err := unmarshalJSON2(row.Metadata, &meta, row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("storage: secret %q: %w", row.Name, err)
		}
		reconcileMeta(&meta, row.ID, row.Name, row.DisplayName)
		out = append(out, catalog.SecretRow{
			ID:              row.ID,
			Name:            row.Name,
			DisplayName:     row.DisplayName,
			Metadata:        meta,
			Spec:            spec,
			ValueKind:       row.ValueKind,
			ValueFromEnv:    row.ValueFromEnv.String,
			ValueFromEnvSet: row.ValueFromEnv.Valid,
			ValueCiphertext: row.ValueCiphertext,
			ValueNonce:      row.ValueNonce,
		})
	}
	return out, nil
}

// ── Secret writes (env-ref and stored) ───────────────────────────────────────
// These are in secrets.go.

// ── Model ─────────────────────────────────────────────────────────────────────

func (r *catalogRepo) UpsertModel(ctx context.Context, m catalog.Model) error {
	meta, spec, err := marshalMetaSpec(m.Metadata, m.Spec)
	if err != nil {
		return fmt.Errorf("storage: UpsertModel %q: %w", m.Metadata.Name, err)
	}
	if err := translateCatalogErr(gen.New(r.db).UpsertModel(ctx, gen.UpsertModelParams{
		ID:          m.Metadata.ID,
		Name:        m.Metadata.Name,
		DisplayName: m.Metadata.DisplayName,
		Metadata:    meta,
		Spec:        spec,
	})); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

func (r *catalogRepo) ListModels(ctx context.Context) ([]catalog.Model, error) {
	rows, err := gen.New(r.db).ListModels(ctx)
	if err != nil {
		return nil, translateCatalogErr(err)
	}
	out := make([]catalog.Model, 0, len(rows))
	for _, row := range rows {
		var meta catalog.Metadata
		var spec catalog.ModelSpec
		if err := unmarshalJSON2(row.Metadata, &meta, row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("storage: model %q: %w", row.Name, err)
		}
		reconcileMeta(&meta, row.ID, row.Name, row.DisplayName)
		out = append(out, catalog.Model{
			APIVersion: catalog.APIVersion,
			Kind:       catalog.KindModel,
			Metadata:   meta,
			Spec:       spec,
		})
	}
	return out, nil
}

func (r *catalogRepo) DeleteModel(ctx context.Context, id string) error {
	if err := translateCatalogErr(gen.New(r.db).DeleteModel(ctx, id)); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// ── Route ─────────────────────────────────────────────────────────────────────

func (r *catalogRepo) UpsertRoute(ctx context.Context, rt catalog.Route) error {
	meta, spec, err := marshalMetaSpec(rt.Metadata, rt.Spec)
	if err != nil {
		return fmt.Errorf("storage: UpsertRoute %q: %w", rt.Metadata.Name, err)
	}
	if err := translateCatalogErr(gen.New(r.db).UpsertRoute(ctx, gen.UpsertRouteParams{
		ID:          rt.Metadata.ID,
		Name:        rt.Metadata.Name,
		DisplayName: rt.Metadata.DisplayName,
		Metadata:    meta,
		Spec:        spec,
	})); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

func (r *catalogRepo) ListRoutes(ctx context.Context) ([]catalog.Route, error) {
	rows, err := gen.New(r.db).ListRoutes(ctx)
	if err != nil {
		return nil, translateCatalogErr(err)
	}
	out := make([]catalog.Route, 0, len(rows))
	for _, row := range rows {
		var meta catalog.Metadata
		var spec catalog.RouteSpec
		if err := unmarshalJSON2(row.Metadata, &meta, row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("storage: route %q: %w", row.Name, err)
		}
		reconcileMeta(&meta, row.ID, row.Name, row.DisplayName)
		out = append(out, catalog.Route{
			APIVersion: catalog.APIVersion,
			Kind:       catalog.KindRoute,
			Metadata:   meta,
			Spec:       spec,
		})
	}
	return out, nil
}

func (r *catalogRepo) DeleteRoute(ctx context.Context, id string) error {
	if err := translateCatalogErr(gen.New(r.db).DeleteRoute(ctx, id)); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// ── RateLimit ─────────────────────────────────────────────────────────────────

func (r *catalogRepo) UpsertRateLimit(ctx context.Context, rl catalog.RateLimit) error {
	meta, spec, err := marshalMetaSpec(rl.Metadata, rl.Spec)
	if err != nil {
		return fmt.Errorf("storage: UpsertRateLimit %q: %w", rl.Metadata.Name, err)
	}
	if err := translateCatalogErr(gen.New(r.db).UpsertRateLimit(ctx, gen.UpsertRateLimitParams{
		ID:          rl.Metadata.ID,
		Name:        rl.Metadata.Name,
		DisplayName: rl.Metadata.DisplayName,
		Metadata:    meta,
		Spec:        spec,
	})); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

func (r *catalogRepo) ListRateLimits(ctx context.Context) ([]catalog.RateLimit, error) {
	rows, err := gen.New(r.db).ListRateLimits(ctx)
	if err != nil {
		return nil, translateCatalogErr(err)
	}
	out := make([]catalog.RateLimit, 0, len(rows))
	for _, row := range rows {
		var meta catalog.Metadata
		var spec catalog.RateLimitSpec
		if err := unmarshalJSON2(row.Metadata, &meta, row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("storage: ratelimit %q: %w", row.Name, err)
		}
		reconcileMeta(&meta, row.ID, row.Name, row.DisplayName)
		out = append(out, catalog.RateLimit{
			APIVersion: catalog.APIVersion,
			Kind:       catalog.KindRateLimit,
			Metadata:   meta,
			Spec:       spec,
		})
	}
	return out, nil
}

func (r *catalogRepo) DeleteRateLimit(ctx context.Context, id string) error {
	if err := translateCatalogErr(gen.New(r.db).DeleteRateLimit(ctx, id)); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// ── UpsertSecret (legacy seed path) ──────────────────────────────────────────

// UpsertSecretRaw writes a secret using the legacy upsert query (YAML seed path).
// The spec must already contain the resolved value_kind fields.
func (r *catalogRepo) UpsertSecretRaw(ctx context.Context, meta catalog.Metadata, spec catalog.SecretSpec) error {
	mb, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretRaw %q: marshal meta: %w", meta.Name, err)
	}
	sb, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("storage: UpsertSecretRaw %q: marshal spec: %w", meta.Name, err)
	}
	if err := translateCatalogErr(gen.New(r.db).UpsertSecret(ctx, gen.UpsertSecretParams{
		ID:          meta.ID,
		Name:        meta.Name,
		DisplayName: meta.DisplayName,
		Metadata:    mb,
		Spec:        sb,
	})); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// ── IsEmpty ───────────────────────────────────────────────────────────────────

// IsEmpty returns true if all catalog tables have zero rows.
// Used by the auto-seed logic.
func (r *catalogRepo) IsEmpty(ctx context.Context) (bool, error) {
	provs, err := r.ListProviders(ctx)
	if err != nil {
		return false, err
	}
	if len(provs) > 0 {
		return false, nil
	}
	policies, err := r.ListPolicies(ctx)
	if err != nil {
		return false, err
	}
	if len(policies) > 0 {
		return false, nil
	}
	secs, err := r.ListSecretRows(ctx)
	if err != nil {
		return false, err
	}
	if len(secs) > 0 {
		return false, nil
	}
	models, err := r.ListModels(ctx)
	if err != nil {
		return false, err
	}
	if len(models) > 0 {
		return false, nil
	}
	routes, err := r.ListRoutes(ctx)
	if err != nil {
		return false, err
	}
	if len(routes) > 0 {
		return false, nil
	}
	rls, err := r.ListRateLimits(ctx)
	if err != nil {
		return false, err
	}
	return len(rls) == 0, nil
}

// ── RelayKey ──────────────────────────────────────────────────────────────────

func (r *catalogRepo) UpsertRelayKey(ctx context.Context, k catalog.RelayKey) error {
	meta, spec, err := marshalMetaSpec(k.Metadata, k.Spec)
	if err != nil {
		return fmt.Errorf("storage: UpsertRelayKey %q: %w", k.Metadata.Name, err)
	}
	if err := translateCatalogErr(gen.New(r.db).UpsertRelayKey(ctx, gen.UpsertRelayKeyParams{
		ID:          k.Metadata.ID,
		Name:        k.Metadata.Name,
		DisplayName: k.Metadata.DisplayName,
		KeyHash:     k.Spec.KeyHash,
		Metadata:    meta,
		Spec:        spec,
	})); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

func (r *catalogRepo) ListRelayKeys(ctx context.Context) ([]catalog.RelayKey, error) {
	rows, err := gen.New(r.db).ListRelayKeys(ctx)
	if err != nil {
		return nil, translateCatalogErr(err)
	}
	out := make([]catalog.RelayKey, 0, len(rows))
	for _, row := range rows {
		var meta catalog.Metadata
		var spec catalog.RelayKeySpec
		if err := unmarshalJSON2(row.Metadata, &meta, row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("storage: relay_key %q: %w", row.Name, err)
		}
		reconcileMeta(&meta, row.ID, row.Name, row.DisplayName)
		// Spec.KeyHash is the source of truth in the JSONB; the column is for
		// the unique index. Reconcile defensively.
		if spec.KeyHash == "" {
			spec.KeyHash = row.KeyHash
		}
		out = append(out, catalog.RelayKey{
			APIVersion: catalog.APIVersion,
			Kind:       catalog.KindRelayKey,
			Metadata:   meta,
			Spec:       spec,
		})
	}
	return out, nil
}

func (r *catalogRepo) DeleteRelayKey(ctx context.Context, id string) error {
	if err := translateCatalogErr(gen.New(r.db).DeleteRelayKey(ctx, id)); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// ── Passthrough (singleton) ──────────────────────────────────────────────────

func (r *catalogRepo) GetPassthrough(ctx context.Context) (*catalog.Passthrough, error) {
	row, err := gen.New(r.db).GetPassthrough(ctx, catalog.PassthroughSingletonName)
	if err != nil {
		if errors.Is(translateCatalogErr(err), catalog.ErrNotFound) {
			return nil, nil
		}
		return nil, translateCatalogErr(err)
	}
	var spec catalog.PassthroughSpec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return nil, fmt.Errorf("storage: passthrough spec unmarshal: %w", err)
	}
	return &catalog.Passthrough{
		APIVersion: catalog.APIVersion,
		Kind:       catalog.KindPassthrough,
		Metadata:   catalog.Metadata{Name: row.Name},
		Spec:       spec,
	}, nil
}

func (r *catalogRepo) SetPassthrough(ctx context.Context, p catalog.Passthrough) error {
	specBytes, err := json.Marshal(p.Spec)
	if err != nil {
		return fmt.Errorf("storage: SetPassthrough marshal: %w", err)
	}
	if err := translateCatalogErr(gen.New(r.db).UpsertPassthrough(ctx, gen.UpsertPassthroughParams{
		Name: catalog.PassthroughSingletonName,
		Spec: specBytes,
	})); err != nil {
		return err
	}
	return r.notifyCatalogChange(ctx)
}

// ── notifyCatalogChange ───────────────────────────────────────────────────────

func (r *catalogRepo) notifyCatalogChange(ctx context.Context) error {
	if _, err := r.db.Exec(ctx, "SELECT pg_notify('relay_catalog', '')"); err != nil {
		slog.Warn("storage: pg_notify relay_catalog failed (non-fatal)", "err", err)
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func marshalMetaSpec(meta, spec any) ([]byte, []byte, error) {
	mb, err := json.Marshal(meta)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal metadata: %w", err)
	}
	sb, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal spec: %w", err)
	}
	return mb, sb, nil
}

func unmarshalJSON2(metaJSON []byte, meta any, specJSON []byte, spec any) error {
	if err := json.Unmarshal(metaJSON, meta); err != nil {
		return fmt.Errorf("unmarshal metadata: %w", err)
	}
	if err := json.Unmarshal(specJSON, spec); err != nil {
		return fmt.Errorf("unmarshal spec: %w", err)
	}
	return nil
}

// reconcileMeta copies authoritative column values into Metadata. The columns
// are PG's source of truth for identity (id PK, name unique, display_name);
// JSONB metadata is a denormalised mirror written by Upsert*. If they ever
// diverge (manual SQL, partial migration), the columns win.
func reconcileMeta(m *catalog.Metadata, id, name, displayName string) {
	m.ID = id
	m.Name = name
	if displayName != "" {
		m.DisplayName = displayName
	}
}
