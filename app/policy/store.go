// store.go is the data-access layer for Policy. Spec.ModelIDs,
// Spec.HostKeyIDs, and Spec.RateLimitID live in their dedicated columns /
// junction tables (migration 0009) — not in the JSONB spec column; so do
// PoolIDs, CredentialIDs, RateLimits and BudgetIDs (migration 0035).
// Upsert fans out across `policies` and every junction inside a single
// transaction so callers see a consistent view.

package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Policy data-access type. Holds a DB rather than Queries so
// Upsert can run a multi-table transaction; List reads without one.
type Store struct {
	db storage.DB
}

// NewStore constructs a Store bound to a pool or an open transaction.
func NewStore(db storage.DB) *Store { return &Store{db: db} }

// List returns every Policy row, hydrating ModelIDs/HostKeyIDs/RateLimitID
// from their relational sources.
func (s *Store) List(ctx context.Context) ([]*Policy, error) {
	q := gen.New(s.db)
	rows, err := q.ListPoliciesWithRateLimit(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy.List: %w", err)
	}
	modelRows, err := q.ListPolicyModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy.List models: %w", err)
	}
	keyRows, err := q.ListPolicyHostKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy.List hostKeys: %w", err)
	}

	modelsByPolicy := map[string][]string{}
	for _, r := range modelRows {
		modelsByPolicy[r.PolicyID] = append(modelsByPolicy[r.PolicyID], r.ModelID)
	}
	keysByPolicy := map[string][]string{}
	for _, r := range keyRows {
		keysByPolicy[r.PolicyID] = append(keysByPolicy[r.PolicyID], r.HostKeyID)
	}

	l, err := listLinks(ctx, q)
	if err != nil {
		return nil, err
	}

	out := make([]*Policy, 0, len(rows))
	for _, r := range rows {
		p, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", r.Name, err)
		}
		p.Spec.ModelIDs = modelsByPolicy[p.Meta.ID]
		p.Spec.HostKeyIDs = keysByPolicy[p.Meta.ID]
		p.Spec.PoolIDs = l.pools[p.Meta.ID]
		p.Spec.CredentialIDs = l.credentials[p.Meta.ID]
		p.Spec.RateLimits = l.rateLimits[p.Meta.ID]
		p.Spec.BudgetIDs = l.budgets[p.Meta.ID]
		out = append(out, p)
	}
	return out, nil
}

// links holds every policy's pool, credential, rate-limit and budget junction rows, keyed by policy id.
type links struct {
	pools, credentials, budgets map[string][]string
	rateLimits                  map[string][]RateLimitLink
}

func listLinks(ctx context.Context, q *gen.Queries) (links, error) {
	l := links{pools: map[string][]string{}, credentials: map[string][]string{}, budgets: map[string][]string{}, rateLimits: map[string][]RateLimitLink{}}
	pools, err := q.ListPolicyPools(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List pools: %w", err)
	}
	for _, r := range pools {
		l.pools[r.PolicyID] = append(l.pools[r.PolicyID], r.PoolID)
	}
	creds, err := q.ListPolicyCredentials(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List credentials: %w", err)
	}
	for _, r := range creds {
		l.credentials[r.PolicyID] = append(l.credentials[r.PolicyID], r.HostKeyID)
	}
	rls, err := q.ListPolicyRateLimits(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List rateLimits: %w", err)
	}
	for _, r := range rls {
		link, err := rateLimitLinkFromRow(r.RateLimitID, r.Models)
		if err != nil {
			return l, fmt.Errorf("policy %s rateLimits: %w", r.PolicyID, err)
		}
		l.rateLimits[r.PolicyID] = append(l.rateLimits[r.PolicyID], link)
	}
	budgets, err := q.ListPolicyBudgets(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List budgets: %w", err)
	}
	for _, r := range budgets {
		l.budgets[r.PolicyID] = append(l.budgets[r.PolicyID], r.BudgetID)
	}
	return l, nil
}

// getLinks fills spec's pool, credential, rate-limit and budget lists for one policy.
func getLinks(ctx context.Context, q *gen.Queries, id string, spec *Spec) error {
	pools, err := q.GetPolicyPools(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get pools: %w", err)
	}
	for _, r := range pools {
		spec.PoolIDs = append(spec.PoolIDs, r.PoolID)
	}
	creds, err := q.GetPolicyCredentials(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get credentials: %w", err)
	}
	for _, r := range creds {
		spec.CredentialIDs = append(spec.CredentialIDs, r.HostKeyID)
	}
	rls, err := q.GetPolicyRateLimits(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get rateLimits: %w", err)
	}
	for _, r := range rls {
		link, err := rateLimitLinkFromRow(r.RateLimitID, r.Models)
		if err != nil {
			return fmt.Errorf("policy.Get rateLimits: %w", err)
		}
		spec.RateLimits = append(spec.RateLimits, link)
	}
	budgets, err := q.GetPolicyBudgets(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get budgets: %w", err)
	}
	for _, r := range budgets {
		spec.BudgetIDs = append(spec.BudgetIDs, r.BudgetID)
	}
	return nil
}

// writeLinks replaces p's pool, credential, rate-limit and budget junction rows; q must be inside the Upsert transaction.
func writeLinks(ctx context.Context, q *gen.Queries, p *Policy) error {
	id := p.Meta.ID
	if err := q.DeletePolicyPools(ctx, id); err != nil {
		return fmt.Errorf("clear pools: %w", err)
	}
	for i, poolID := range p.Spec.PoolIDs {
		if err := q.InsertPolicyPool(ctx, gen.InsertPolicyPoolParams{PolicyID: id, PoolID: poolID, Position: int32(i)}); err != nil {
			return fmt.Errorf("insert pool %s: %w", poolID, err)
		}
	}
	if err := q.DeletePolicyCredentials(ctx, id); err != nil {
		return fmt.Errorf("clear credentials: %w", err)
	}
	for i, keyID := range p.Spec.CredentialIDs {
		if err := q.InsertPolicyCredential(ctx, gen.InsertPolicyCredentialParams{PolicyID: id, HostKeyID: keyID, Position: int32(i)}); err != nil {
			return fmt.Errorf("insert credential %s: %w", keyID, err)
		}
	}
	if err := q.DeletePolicyRateLimits(ctx, id); err != nil {
		return fmt.Errorf("clear rateLimits: %w", err)
	}
	for i, l := range p.Spec.RateLimits {
		models := []byte("[]")
		if len(l.Models) > 0 {
			var err error
			if models, err = json.Marshal(l.Models); err != nil {
				return fmt.Errorf("rateLimits[%d] models: %w", i, err)
			}
		}
		if err := q.InsertPolicyRateLimit(ctx, gen.InsertPolicyRateLimitParams{PolicyID: id, RateLimitID: l.RateLimitID, Models: models, Position: int32(i)}); err != nil {
			return fmt.Errorf("insert rate limit %s: %w", l.RateLimitID, err)
		}
	}
	if err := q.DeletePolicyBudgets(ctx, id); err != nil {
		return fmt.Errorf("clear budgets: %w", err)
	}
	for i, budgetID := range p.Spec.BudgetIDs {
		if err := q.InsertPolicyBudget(ctx, gen.InsertPolicyBudgetParams{PolicyID: id, BudgetID: budgetID, Position: int32(i)}); err != nil {
			return fmt.Errorf("insert budget %s: %w", budgetID, err)
		}
	}
	return nil
}

func rateLimitLinkFromRow(rateLimitID string, models []byte) (RateLimitLink, error) {
	link := RateLimitLink{RateLimitID: rateLimitID}
	if err := json.Unmarshal(models, &link.Models); err != nil {
		return link, fmt.Errorf("models: %w", err)
	}
	// An unscoped link reads back as nil, as it was written.
	if len(link.Models) == 0 {
		link.Models = nil
	}
	return link, nil
}

// Get returns the Policy with the given id, hydrating ModelIDs/HostKeyIDs/RateLimitID.
// Returns (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Policy, error) {
	q := gen.New(s.db)
	r, err := q.GetPolicy(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("policy.Get: %w", err)
	}
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	if r.RateLimitID.Valid {
		spec.RateLimitID = r.RateLimitID.String
	}
	if len(r.Models) > 0 {
		if err := json.Unmarshal(r.Models, &spec.Models); err != nil {
			return nil, fmt.Errorf("models: %w", err)
		}
	}
	modelRows, err := q.GetPolicyModels(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("policy.Get models: %w", err)
	}
	for _, mr := range modelRows {
		spec.ModelIDs = append(spec.ModelIDs, mr.ModelID)
	}
	keyRows, err := q.GetPolicyHostKeys(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("policy.Get hostkeys: %w", err)
	}
	for _, kr := range keyRows {
		spec.HostKeyIDs = append(spec.HostKeyIDs, kr.HostKeyID)
	}
	if err := getLinks(ctx, q, id, &spec); err != nil {
		return nil, err
	}
	return &Policy{Meta: md, Spec: spec}, nil
}

// Upsert writes p across policies + junction tables in a single tx.
func (s *Store) Upsert(ctx context.Context, p *Policy) error {
	params, err := toUpsertParams(p)
	if err != nil {
		return fmt.Errorf("policy.Upsert: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("policy.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := meta.StaleIfNoRows(q.UpsertPolicy(ctx, params)); err != nil {
		return fmt.Errorf("policy.Upsert: policies: %w", err)
	}
	rateLimitID := pgtype.Text{}
	if p.Spec.RateLimitID != "" {
		rateLimitID = pgtype.Text{String: p.Spec.RateLimitID, Valid: true}
	}
	if err := q.SetPolicyRateLimit(ctx, gen.SetPolicyRateLimitParams{
		ID:          p.Meta.ID,
		RateLimitID: rateLimitID,
	}); err != nil {
		return fmt.Errorf("policy.Upsert: rate_limit_id: %w", err)
	}
	if err := q.DeletePolicyModels(ctx, p.Meta.ID); err != nil {
		return fmt.Errorf("policy.Upsert: clear models: %w", err)
	}
	for i, id := range p.Spec.ModelIDs {
		if err := q.InsertPolicyModel(ctx, gen.InsertPolicyModelParams{
			PolicyID: p.Meta.ID,
			ModelID:  id,
			Position: int32(i),
		}); err != nil {
			return fmt.Errorf("policy.Upsert: insert model %s: %w", id, err)
		}
	}
	if err := q.DeletePolicyHostKeys(ctx, p.Meta.ID); err != nil {
		return fmt.Errorf("policy.Upsert: clear hostKeys: %w", err)
	}
	for i, id := range p.Spec.HostKeyIDs {
		if err := q.InsertPolicyHostKey(ctx, gen.InsertPolicyHostKeyParams{
			PolicyID:  p.Meta.ID,
			HostKeyID: id,
			Position:  int32(i),
		}); err != nil {
			return fmt.Errorf("policy.Upsert: insert hostKey %s: %w", id, err)
		}
	}
	if err := writeLinks(ctx, q, p); err != nil {
		return fmt.Errorf("policy.Upsert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("policy.Upsert: commit: %w", err)
	}
	return nil
}

// Delete removes a Policy by id. Junction rows cascade via FK.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.db).DeletePolicy(ctx, id)
}

func fromRow(r gen.ListPoliciesWithRateLimitRow) (*Policy, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	if r.RateLimitID.Valid {
		spec.RateLimitID = r.RateLimitID.String
	}
	if len(r.Models) > 0 {
		if err := json.Unmarshal(r.Models, &spec.Models); err != nil {
			return nil, fmt.Errorf("models: %w", err)
		}
	}
	return &Policy{Meta: md, Spec: spec}, nil
}

func toUpsertParams(p *Policy) (gen.UpsertPolicyParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertPolicyParams{}, fmt.Errorf("metadata: %w", err)
	}
	// Strip relational fields from the JSONB spec — they live in columns / junctions now.
	specCopy := p.Spec
	specCopy.ModelIDs = nil
	specCopy.HostKeyIDs = nil
	specCopy.RateLimitID = ""
	specCopy.Models = nil // models lives in its own column
	specCopy.PoolIDs = nil
	specCopy.CredentialIDs = nil
	specCopy.RateLimits = nil
	specCopy.BudgetIDs = nil
	specJSON, err := json.Marshal(specCopy)
	if err != nil {
		return gen.UpsertPolicyParams{}, fmt.Errorf("spec: %w", err)
	}
	modelsJSON := []byte("[]")
	if len(p.Spec.Models) > 0 {
		modelsJSON, err = json.Marshal(p.Spec.Models)
		if err != nil {
			return gen.UpsertPolicyParams{}, fmt.Errorf("models: %w", err)
		}
	}
	ver, ok, err := p.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertPolicyParams{}, err
	}
	return gen.UpsertPolicyParams{
		ID:              p.Meta.ID,
		Name:            p.Meta.Name,
		DisplayName:     p.Meta.DisplayName,
		Metadata:        metaJSON,
		Spec:            specJSON,
		Models:          modelsJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
