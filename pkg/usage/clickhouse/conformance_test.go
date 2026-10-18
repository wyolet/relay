//go:build integration

package clickhouse

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/pkg/usage/usagetest"
)

func TestIntegration_Conformance(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse conformance")
	}
	usagetest.Run(t, func(t *testing.T, events []usage.Event) usage.Reader {
		s, err := New(Config{DSN: scratchDatabase(t, dsn), WALDir: t.TempDir()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		// insertBatch is synchronous; Write would leave the rows to the WAL flush.
		if err := s.insertBatch(events); err != nil {
			t.Fatalf("insertBatch: %v", err)
		}
		return s
	})
}

// scratchDatabase creates a database dropped at cleanup and returns dsn pointed at it. usage_events is addressed unqualified, so the fixture's fixed timestamps would otherwise mix with every other row on the server.
func scratchDatabase(t *testing.T, dsn string) string {
	t.Helper()
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	admin, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db := fmt.Sprintf("relay_conformance_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		defer admin.Close()
		if err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db); err != nil {
			t.Errorf("cleanup: drop database %s: %v", db, err)
		}
	})
	if err := admin.Exec(context.Background(), "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn url: %v", err)
	}
	u.Path = "/" + db
	return u.String()
}
