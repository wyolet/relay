// Package valkey is a TTL-bounded usage Sink + Reader backed by pkg/kv.
// It is a hot-window / live-tail cache — NOT long-term history. Events expire
// automatically via the TTL set on each key; ClickHouse owns history.
//
// Expected kv ops per Write: 1 × Set (one key, JSON value, TTL-bounded).
// Expected kv ops per Events/Summary query: 1 × Range (prefix scan).
//
// All keys share the hash-tag "{u}" so every key touched in a Range scan
// maps to the same Redis Cluster slot and cluster-mode Range works correctly.
// Key schema: usagevk:{u}:<20-digit-zero-padded-unixnano>:<request_id>
// Lexical order of the timestamp portion approximates time order, but we
// sort in Go after decode — do not rely on store ordering.
package valkey

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/usage"
)

// store is the narrow kv interface this package uses. Declare only what we
// need — callers can pass any kv.Store implementation.
type store interface {
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Range(ctx context.Context, prefix string) ([]kv.Entry, error)
}

// Config controls runtime behaviour.
type Config struct {
	// TTL is the per-event key lifetime. Defaults to 24h when zero.
	TTL time.Duration
	// KeyPrefix is the Redis key prefix (without the hash-tag). Defaults
	// to "usagevk". Override in tests that share a store.
	KeyPrefix string
}

func (c *Config) ttl() time.Duration {
	if c.TTL <= 0 {
		return 24 * time.Hour
	}
	return c.TTL
}

func (c *Config) prefix() string {
	if c.KeyPrefix == "" {
		return "usagevk"
	}
	return c.KeyPrefix
}

// Sink is the valkey backend. It implements usage.Sink and usage.Reader.
type Sink struct {
	s   store
	cfg Config
}

var _ usage.Sink = (*Sink)(nil)
var _ usage.Reader = (*Sink)(nil)

// New constructs a Sink. s must implement the narrow store interface (any
// kv.Store satisfies it). cfg fields are optional; zero values use defaults.
func New(s store, cfg Config) *Sink {
	return &Sink{s: s, cfg: cfg}
}

// Write marshals ev to JSON and stores it under a TTL-bounded key.
func (sk *Sink) Write(ev usage.Event) error {
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	key := eventKey(sk.cfg.prefix(), ts, ev.RequestID)
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("valkey.Write: marshal: %w", err)
	}
	return sk.s.Set(context.Background(), key, data, sk.cfg.ttl())
}

// Events returns raw events matching q, newest-first, capped at q.Limit.
func (sk *Sink) Events(ctx context.Context, q usage.EventQuery) ([]usage.Event, error) {
	all, err := sk.scan(ctx)
	if err != nil {
		return nil, err
	}
	return usage.SortAndLimit(usage.FilterEvents(all, q), q.Limit), nil
}

// Summary returns aggregated rows grouped by q.GroupBy.
func (sk *Sink) Summary(ctx context.Context, q usage.SummaryQuery) (usage.SummaryResult, error) {
	all, err := sk.scan(ctx)
	if err != nil {
		return usage.SummaryResult{}, err
	}
	return usage.Summarize(usage.FilterEvents(all, q.EventQuery), q.GroupBy)
}

// TimeSeries buckets the matching events into time series via
// usage.Bucketize.
func (sk *Sink) TimeSeries(ctx context.Context, q usage.TimeSeriesQuery) (usage.TimeSeriesResult, error) {
	all, err := sk.scan(ctx)
	if err != nil {
		return usage.TimeSeriesResult{}, err
	}
	res, err := usage.Bucketize(usage.FilterEvents(all, q.EventQuery), q.Interval, q.GroupBy)
	if err != nil {
		return usage.TimeSeriesResult{}, err
	}
	res.Interval = q.Interval.String()
	return res, nil
}

// scan fetches and decodes all events from the store, skipping malformed
// values (so a single bad entry never breaks a query).
func (sk *Sink) scan(ctx context.Context) ([]usage.Event, error) {
	prefix := scanPrefix(sk.cfg.prefix())
	entries, err := sk.s.Range(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("valkey.scan: range: %w", err)
	}
	events := make([]usage.Event, 0, len(entries))
	for _, e := range entries {
		var ev usage.Event
		if json.Unmarshal(e.Value, &ev) != nil {
			continue // skip malformed
		}
		events = append(events, ev)
	}
	return events, nil
}
