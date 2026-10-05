// Package chttl keeps a ClickHouse MergeTree table's row TTL equal to a
// retention setting. CREATE TABLE ... TTL only applies when the table is
// created, so a retention changed later never reached an existing table;
// sinks call Apply on every build instead. Out of scope: column TTLs and
// TTLs with MOVE/RECOMPRESS actions.
package chttl

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Apply sets table's TTL to days after tsColumn, or removes it when days is
// 0 (keep forever). A table already at the requested TTL is left untouched,
// so the call from every pod and every sink rebuild costs one system.tables
// read. Shortening the TTL deletes expired rows in a background mutation.
func Apply(ctx context.Context, conn clickhouse.Conn, table, tsColumn string, days int) error {
	if days < 0 {
		return fmt.Errorf("chttl: %s: retention days must be >= 0, got %d", table, days)
	}
	current, err := currentDays(ctx, conn, table, tsColumn)
	if err != nil {
		return err
	}
	if current == days {
		return nil
	}
	stmt := fmt.Sprintf("ALTER TABLE %s MODIFY TTL toDateTime(%s) + INTERVAL %d DAY", table, tsColumn, days)
	if days == 0 {
		stmt = "ALTER TABLE " + table + " REMOVE TTL"
	}
	if err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("chttl: %s: %w", table, err)
	}
	return nil
}

// currentDays reads the table's TTL in days: 0 when it has none, -1 when it
// has one this package did not write (so Apply replaces it).
func currentDays(ctx context.Context, conn clickhouse.Conn, table, tsColumn string) (int, error) {
	var engine string
	row := conn.QueryRow(ctx,
		"SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = ?", table)
	if err := row.Scan(&engine); err != nil {
		return 0, fmt.Errorf("chttl: %s: read engine: %w", table, err)
	}
	return parseDays(engine, tsColumn), nil
}

func parseDays(engine, tsColumn string) int {
	if !strings.Contains(engine, " TTL ") {
		return 0
	}
	re := regexp.MustCompile(`TTL toDateTime\(` + regexp.QuoteMeta(tsColumn) + `\) \+ toIntervalDay\((\d+)\)( SETTINGS|$)`)
	m := re.FindStringSubmatch(engine)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}
