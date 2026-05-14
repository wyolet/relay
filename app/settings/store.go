package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Row is a single settings row, decoded into a typed value via its
// section's Decode func. UpdatedAt is informational.
type Row struct {
	Section   string
	Value     any
	UpdatedAt time.Time
}

// Store is the data-access layer for settings rows.
type Store struct {
	q *gen.Queries
}

func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// Get returns the typed value for section. When no row exists, the
// section's Defaults() is returned with UpdatedAt zero — so the read
// path is total: every registered section always has a value.
func (s *Store) Get(ctx context.Context, section string) (*Row, error) {
	sec, ok := Lookup(section)
	if !ok {
		return nil, fmt.Errorf("unknown section %q", section)
	}
	r, err := s.q.GetSetting(ctx, section)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &Row{Section: section, Value: sec.Defaults()}, nil
		}
		return nil, fmt.Errorf("settings.Get: %w", err)
	}
	v, err := sec.Decode(r.Value)
	if err != nil {
		return nil, fmt.Errorf("settings.Get %s: %w", section, err)
	}
	return &Row{Section: section, Value: v, UpdatedAt: r.UpdatedAt.Time}, nil
}

// Upsert writes value as the typed section. The Decode round-trip
// validates input before any DB write.
func (s *Store) Upsert(ctx context.Context, section string, raw json.RawMessage) (*Row, error) {
	sec, ok := Lookup(section)
	if !ok {
		return nil, fmt.Errorf("unknown section %q", section)
	}
	v, err := sec.Decode(raw)
	if err != nil {
		return nil, err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("re-marshal: %w", err)
	}
	if err := s.q.UpsertSetting(ctx, gen.UpsertSettingParams{
		Section: section,
		Value:   canon,
	}); err != nil {
		return nil, fmt.Errorf("settings.Upsert: %w", err)
	}
	return &Row{Section: section, Value: v, UpdatedAt: time.Now()}, nil
}

// List returns one Row per registered section, falling back to
// Defaults for sections without a DB row.
func (s *Store) List(ctx context.Context) ([]*Row, error) {
	rows, err := s.q.ListSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("settings.List: %w", err)
	}
	byName := make(map[string]gen.Setting, len(rows))
	for _, r := range rows {
		byName[r.Section] = r
	}
	names := Names()
	out := make([]*Row, 0, len(names))
	for _, n := range names {
		sec, _ := Lookup(n)
		if r, ok := byName[n]; ok {
			v, err := sec.Decode(r.Value)
			if err != nil {
				return nil, fmt.Errorf("settings.List %s: %w", n, err)
			}
			out = append(out, &Row{Section: n, Value: v, UpdatedAt: r.UpdatedAt.Time})
			continue
		}
		out = append(out, &Row{Section: n, Value: sec.Defaults()})
	}
	return out, nil
}
