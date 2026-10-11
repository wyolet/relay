package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/auth/scim"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

// SCIMTokens is the scim.TokenStore over the scim_clients table.
type SCIMTokens struct{ db DB }

// NewSCIMTokens returns the SCIM client store over db.
func NewSCIMTokens(db DB) *SCIMTokens { return &SCIMTokens{db: db} }

var _ scim.TokenStore = (*SCIMTokens)(nil)

func (s *SCIMTokens) ClientByTokenHash(ctx context.Context, hash string) (*scim.Client, error) {
	r, err := gen.New(s.db).GetSCIMClientByTokenHash(ctx, hash)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get scim client", err)
	}
	return scimClientFromRow(r), nil
}

func (s *SCIMTokens) MarkUsed(ctx context.Context, name string, t time.Time) error {
	return mapErr("mark scim client used", gen.New(s.db).MarkSCIMClientUsed(ctx, gen.MarkSCIMClientUsedParams{
		Name: name, UsedAt: pgtype.Timestamptz{Time: t, Valid: true},
	}))
}

func (s *SCIMTokens) CreateClient(ctx context.Context, c *scim.Client) error {
	n, err := gen.New(s.db).CreateSCIMClient(ctx, gen.CreateSCIMClientParams{Name: c.Name, TokenHash: c.TokenHash, TokenPrefix: c.TokenPrefix})
	if err != nil {
		return mapErr("create scim client", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %q", scim.ErrClientExists, c.Name)
	}
	return nil
}

func (s *SCIMTokens) ListClients(ctx context.Context) ([]*scim.Client, error) {
	rows, err := gen.New(s.db).ListSCIMClients(ctx)
	if err != nil {
		return nil, mapErr("list scim clients", err)
	}
	out := make([]*scim.Client, len(rows))
	for i, r := range rows {
		out[i] = scimClientFromRow(r)
	}
	return out, nil
}

func (s *SCIMTokens) RevokeClient(ctx context.Context, name string) error {
	q := gen.New(s.db)
	n, err := q.RevokeSCIMClient(ctx, name)
	if err != nil {
		return mapErr("revoke scim client", err)
	}
	if n > 0 {
		return nil
	}
	// No live row changed: either it is revoked already, which is fine, or there is none.
	if _, err := q.GetSCIMClient(ctx, name); notFound(err) {
		return fmt.Errorf("%w: %q", scim.ErrNoClient, name)
	} else if err != nil {
		return mapErr("get scim client", err)
	}
	return nil
}

func scimClientFromRow(r gen.ScimClient) *scim.Client {
	return &scim.Client{
		Name: r.Name, TokenHash: r.TokenHash, TokenPrefix: r.TokenPrefix,
		CreatedAt: r.CreatedAt.Time, LastUsedAt: r.LastUsedAt.Time, Revoked: r.RevokedAt.Valid,
	}
}
