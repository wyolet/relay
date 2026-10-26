package clickhouse

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/wyolet/relay/pkg/usage"
)

// nativeInserter sends batches over its own ch-go connection: clickhouse-go's
// PrepareBatch builds fresh columns for every INSERT, while ch-go takes
// caller-owned ones. Reads and DDL stay on the clickhouse-go pool.
type nativeInserter struct {
	// dial holds one entry per DSN host, tried in order.
	dial     []ch.Options
	lifetime time.Duration

	mu     sync.Mutex
	client *ch.Client
	dialed time.Time
	cols   *eventColumns
}

// clickhouse-go's defaults when the DSN leaves them unset; ch-go's are 1s and
// 3s, too short for an async insert that waits for the server's flush.
const (
	defaultDialTimeout = 30 * time.Second
	defaultReadTimeout = 300 * time.Second
)

// newNativeInserter reuses the clickhouse-go DSN options. database is the
// server-resolved current database, so inserts land where ensureSchema
// created the table even when the DSN names none.
func newNativeInserter(opts *clickhouse.Options, database string) *nativeInserter {
	base := ch.Options{
		Database:    database,
		User:        opts.Auth.Username,
		Password:    opts.Auth.Password,
		TLS:         opts.TLS,
		DialTimeout: cmp.Or(opts.DialTimeout, defaultDialTimeout),
		ReadTimeout: cmp.Or(opts.ReadTimeout, defaultReadTimeout),
		Compression: ch.CompressionNone,
	}
	if c := opts.Compression; c != nil {
		switch c.Method {
		case clickhouse.CompressionLZ4:
			base.Compression = ch.CompressionLZ4
		case clickhouse.CompressionLZ4HC:
			base.Compression = ch.CompressionLZ4HC
		case clickhouse.CompressionZSTD:
			base.Compression = ch.CompressionZSTD
		}
		base.CompressionLevel = ch.CompressionLevel(c.Level)
	}
	for k, v := range opts.Settings {
		s := ch.Setting{Key: k, Important: true}
		if cv, ok := v.(clickhouse.CustomSetting); ok {
			s.Value, s.Important = cv.Value, false
		} else {
			s.Value = fmt.Sprint(v)
		}
		base.Settings = append(base.Settings, s)
	}

	addrs := opts.Addr
	if len(addrs) == 0 {
		addrs = []string{"localhost:9000"}
	}
	dial := make([]ch.Options, len(addrs))
	for i, addr := range addrs {
		dial[i] = base
		dial[i].Address = addr
	}
	return &nativeInserter{
		dial:     dial,
		lifetime: cmp.Or(opts.ConnMaxLifetime, time.Hour),
		cols:     newEventColumns(),
	}
}

func (n *nativeInserter) insert(ctx context.Context, events []usage.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	client, err := n.connection(ctx)
	if err != nil {
		return err
	}
	defer n.cols.reset()
	n.cols.appendEvents(events)
	if err := client.Do(ctx, ch.Query{Body: insertSQL + " VALUES", Input: n.cols.input}); err != nil {
		n.closeClient()
		return fmt.Errorf("insert: %w", err)
	}
	return nil
}

// connection returns a live client, redialling when the current one is past
// its lifetime or the server dropped it while idle.
func (n *nativeInserter) connection(ctx context.Context) (*ch.Client, error) {
	if n.client != nil && (time.Since(n.dialed) > n.lifetime || n.client.Ping(ctx) != nil) {
		n.closeClient()
	}
	if n.client != nil {
		return n.client, nil
	}
	var errs []error
	for _, opt := range n.dial {
		client, err := ch.Dial(ctx, opt)
		if err == nil {
			n.client, n.dialed = client, time.Now()
			return client, nil
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("dial: %w", errors.Join(errs...))
}

func (n *nativeInserter) closeClient() {
	if n.client != nil {
		_ = n.client.Close()
		n.client = nil
	}
}

func (n *nativeInserter) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closeClient()
}
