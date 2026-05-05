//go:build integration

package eventlog_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/wyolet/relay/pkg/eventlog"
)

func startClickHouse(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	req := tc.ContainerRequest{
		Image:        "clickhouse/clickhouse-server:latest",
		ExposedPorts: []string{"9000/tcp", "8123/tcp"},
		Env: map[string]string{
			"CLICKHOUSE_ALLOW_EMPTY_PASSWORD": "1",
			"CLICKHOUSE_USER":                 "default",
			"CLICKHOUSE_PASSWORD":             "",
			"CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1",
		},
		WaitingFor: wait.ForHTTP("/ping").WithPort("8123/tcp").WithStartupTimeout(90 * time.Second),
	}
	ctr, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start clickhouse container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("clickhouse host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "9000")
	if err != nil {
		t.Fatalf("clickhouse port: %v", err)
	}
	return fmt.Sprintf("clickhouse://default:@%s:%s/default", host, port.Port())
}

func makeEvent(i int) eventlog.Event {
	return eventlog.Event{
		EventVersion: 1,
		RequestID:    fmt.Sprintf("req-%04d", i),
		Model:        "gpt-4o",
		Provider:     "openai",
		Pool:         "pool-a",
		SecretHash:   "abc123def456",
		TerminatedBy: "clean",
		Tokens:       eventlog.TokenCounts{Prompt: 10, Completion: 20, Total: 30},
		Attribution:  map[string]string{"user": "u1"},
		Metrics:      map[string]int64{"retry": 0},
		InstanceID:   "pod-1",
		RelayVersion: "v0.1.0",
		StartedAt:    time.Now().UTC().Format("2006-01-02T15:04:05.999999999Z"),
		EndedAt:      time.Now().UTC().Format("2006-01-02T15:04:05.999999999Z"),
	}
}

func TestClickHouseSink_Basic(t *testing.T) {
	dsn := startClickHouse(t)
	ctx := context.Background()

	l, err := eventlog.New(eventlog.Config{
		Backend:     eventlog.BackendClickHouse,
		DSN:         dsn,
		BufferSize:  256,
		FlushPeriod: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 100; i++ {
		if err := l.Append(ctx, makeEvent(i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	closeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := l.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Query back.
	conn := openConn(t, dsn)

	var count uint64
	var uniq uint64
	if err := conn.QueryRow(ctx, "SELECT count(), uniqExact(request_id) FROM usage_events").Scan(&count, &uniq); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 100 {
		t.Errorf("count = %d, want 100", count)
	}
	if uniq != 100 {
		t.Errorf("uniqExact(request_id) = %d, want 100", uniq)
	}
}

func TestClickHouseSink_SchemaIdempotency(t *testing.T) {
	dsn := startClickHouse(t)

	cfg := eventlog.Config{
		Backend:     eventlog.BackendClickHouse,
		DSN:         dsn,
		BufferSize:  64,
		FlushPeriod: time.Second,
	}

	l1, err := eventlog.New(cfg)
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	if err := l1.Close(context.Background()); err != nil {
		t.Fatalf("Close first: %v", err)
	}

	l2, err := eventlog.New(cfg)
	if err != nil {
		t.Fatalf("New second (idempotency): %v", err)
	}
	if err := l2.Close(context.Background()); err != nil {
		t.Fatalf("Close second: %v", err)
	}
}

func TestClickHouseSink_DropSemantics(t *testing.T) {
	dsn := startClickHouse(t)
	ctx := context.Background()

	l, err := eventlog.New(eventlog.Config{
		Backend:     eventlog.BackendClickHouse,
		DSN:         dsn,
		BufferSize:  2,
		FlushPeriod: time.Hour, // prevent drain
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer l.Close(ctx)

	for i := 0; i < 20; i++ {
		l.Append(ctx, makeEvent(i))
	}

	s := l.Stats()
	if s.Dropped == 0 {
		t.Error("expected Dropped > 0 with tiny buffer")
	}
}

func TestClickHouseSink_SchemaAdditive(t *testing.T) {
	dsn := startClickHouse(t)
	ctx := context.Background()

	// Boot once to create the table.
	l, err := eventlog.New(eventlog.Config{
		Backend:     eventlog.BackendClickHouse,
		DSN:         dsn,
		BufferSize:  64,
		FlushPeriod: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l.Append(ctx, makeEvent(0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	l.Close(ctx)

	// Add a column externally.
	{
		conn := openConn(t, dsn)
		if err := conn.Exec(ctx, "ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS extra_col String DEFAULT ''"); err != nil {
			t.Fatalf("alter table: %v", err)
		}
	}

	// Write again without any code change — should succeed.
	l2, err := eventlog.New(eventlog.Config{
		Backend:     eventlog.BackendClickHouse,
		DSN:         dsn,
		BufferSize:  64,
		FlushPeriod: time.Second,
	})
	if err != nil {
		t.Fatalf("New after alter: %v", err)
	}
	if err := l2.Append(ctx, makeEvent(1)); err != nil {
		t.Fatalf("Append after alter: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := l2.Close(closeCtx); err != nil {
		t.Fatalf("Close after alter: %v", err)
	}
}

func TestClickHouseSink_RetentionDays(t *testing.T) {
	dsn := startClickHouse(t)
	ctx := context.Background()

	l, err := eventlog.New(eventlog.Config{
		Backend:       eventlog.BackendClickHouse,
		DSN:           dsn,
		BufferSize:    64,
		FlushPeriod:   time.Second,
		RetentionDays: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Close(ctx)

	conn := openConn(t, dsn)

	// SHOW CREATE TABLE includes the TTL clause.
	rows, err := conn.Query(ctx, "SHOW CREATE TABLE usage_events")
	if err != nil {
		t.Fatalf("show create table: %v", err)
	}
	var createStmt string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		createStmt += s
	}
	rows.Close()

	if createStmt == "" {
		t.Fatal("empty CREATE TABLE statement")
	}
	// CH normalizes TTL to toIntervalDay(N); check the retention value appears.
	if !containsAll(createStmt, "TTL") {
		t.Errorf("CREATE TABLE DDL missing TTL clause: %s", createStmt)
	}
	if !containsAll(createStmt, "toIntervalDay(1)") && !containsAll(createStmt, "INTERVAL 1 DAY") {
		t.Errorf("CREATE TABLE DDL doesn't mention retention day 1: %s", createStmt)
	}
}

func TestClickHouseSink_Ping(t *testing.T) {
	dsn := startClickHouse(t)

	l, err := eventlog.New(eventlog.Config{
		Backend:     eventlog.BackendClickHouse,
		DSN:         dsn,
		BufferSize:  64,
		FlushPeriod: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer l.Close(context.Background())

	if err := l.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// containsAll checks if s contains all the given substrings.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// openConn opens a raw clickhouse connection using the same DSN.
func openConn(t *testing.T, dsn string) clickhouse.Conn {
	t.Helper()
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatalf("open ch: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}
