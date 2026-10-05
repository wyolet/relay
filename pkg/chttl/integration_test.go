//go:build integration

// Live ClickHouse check. Runs only with -tags=integration AND
// RELAY_TEST_CH_DSN set (else skipped).
//
//	RELAY_TEST_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/chttl/ -v

package chttl

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func TestIntegration_Apply(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse check")
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	ctx := context.Background()
	db := fmt.Sprintf("relay_chttl_test_%d", time.Now().UnixNano())

	admin, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer admin.Close()
	if err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db) })

	scoped := *opts
	scoped.Auth.Database = db
	conn, err := clickhouse.Open(&scoped)
	if err != nil {
		t.Fatalf("open %s: %v", db, err)
	}
	defer conn.Close()

	// The DDL relay shipped before retention became a setting.
	if err := conn.Exec(ctx, `CREATE TABLE events (ts DateTime64(9, 'UTC'), v String)
ENGINE = MergeTree PARTITION BY toYYYYMMDD(ts) ORDER BY ts
TTL toDateTime(ts) + INTERVAL 30 DAY`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	for _, days := range []int{30, 0, 0, 7, 7, 90, 0} {
		if err := Apply(ctx, conn, "events", "ts", days); err != nil {
			t.Fatalf("Apply(%d): %v", days, err)
		}
		got, err := currentDays(ctx, conn, "events", "ts")
		if err != nil {
			t.Fatalf("currentDays: %v", err)
		}
		if got != days {
			t.Fatalf("after Apply(%d): table TTL = %d days", days, got)
		}
	}
}
