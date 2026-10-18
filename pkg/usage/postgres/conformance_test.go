//go:build integration

package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/pkg/usage/usagetest"
)

func TestIntegration_Conformance(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN unset; skipping live PostgreSQL conformance")
	}
	usagetest.Run(t, func(t *testing.T, events []usage.Event) usage.Reader {
		ctx := context.Background()
		table := fmt.Sprintf("usage_events_conformance_%d", time.Now().UnixNano())
		s, err := New(ctx, Config{DSN: dsn, Table: table})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		// Cleanups run last-registered first: the table is dropped while the pool is still open.
		t.Cleanup(func() { _ = s.Close() })
		t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
		// insertBatch is synchronous; Write would leave the rows to the flush ticker.
		if err := s.insertBatch(events); err != nil {
			t.Fatalf("insertBatch: %v", err)
		}
		return s
	})
}
