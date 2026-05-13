// store.go is the data-access layer for Policy.
//
// TODO(arch): the target storage shape uses junction tables
// (policy_models, policy_provider_keys) plus a policies.rate_limit_id
// column. That requires migrations + new sqlc queries which haven't
// landed yet, so for now Spec.ModelIDs, Spec.ProviderKeyIDs, and
// Spec.RateLimitID continue to round-trip inside the existing JSONB
// spec column. When the junctions land, fromRow becomes a List+JOIN
// and toUpsertParams becomes a multi-table transaction; Spec stops
// carrying these fields in JSONB.
package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Policy data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Policy row.
func (s *Store) List(ctx context.Context) ([]*Policy, error) {
	rows, err := s.q.ListPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy.List: %w", err)
	}
	out := make([]*Policy, 0, len(rows))
	for _, r := range rows {
		p, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", r.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Upsert writes p. Caller is responsible for stamping Meta.ID.
func (s *Store) Upsert(ctx context.Context, p *Policy) error {
	params, err := toUpsertParams(p)
	if err != nil {
		return fmt.Errorf("policy.Upsert: %w", err)
	}
	return s.q.UpsertPolicy(ctx, params)
}

// Delete removes a Policy by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeletePolicy(ctx, id)
}

func fromRow(r gen.ListPoliciesRow) (*Policy, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &Policy{Meta: md, Spec: spec}, nil
}

func toUpsertParams(p *Policy) (gen.UpsertPolicyParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertPolicyParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(p.Spec)
	if err != nil {
		return gen.UpsertPolicyParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertPolicyParams{
		ID:          p.Meta.ID,
		Name:        p.Meta.Name,
		DisplayName: p.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
