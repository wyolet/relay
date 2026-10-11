//go:build integration

package postgres

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The product in this repository created the directory tables before the module existed. These tests prove the module's migration leaves its schema alone, that the two schemas agree, and that the module reads and writes the rows it has.

var directoryTables = []string{
	"users", "teams", "projects", "service_accounts", "groups", "group_members",
	"roles", "role_bindings", "role_binding_subjects",
}

// productOnly are the schema entries the product adds to the shared tables and the module deliberately leaves out.
var productOnly = []string{
	"con role_bindings role_bindings_scope_kind_check",
}

const schemaQuery = `
SELECT 'col ' || table_name || ' ' || ordinal_position || ' ' || column_name || ' ' || data_type || ' ' ||
       is_nullable || ' ' || coalesce(column_default, '')
  FROM information_schema.columns
 WHERE table_schema = current_schema() AND table_name = ANY($1)
UNION ALL
SELECT 'idx ' || tablename || ' ' || indexname || ' ' || indexdef
  FROM pg_indexes WHERE schemaname = current_schema() AND tablename = ANY($1)
UNION ALL
SELECT 'con ' || c.conrelid::regclass::text || ' ' || c.conname || ' ' || pg_get_constraintdef(c.oid)
  FROM pg_constraint c
 WHERE c.connamespace = current_schema()::regnamespace AND c.conrelid::regclass::text = ANY($1)
UNION ALL
SELECT 'trg ' || t.tgrelid::regclass::text || ' ' || t.tgname || ' ' || pg_get_triggerdef(t.oid)
  FROM pg_trigger t
 WHERE NOT t.tgisinternal AND t.tgrelid::regclass::text = ANY($1)
UNION ALL
SELECT 'fn ' || p.proname || ' ' || md5(p.prosrc)
  FROM pg_proc p WHERE p.pronamespace = current_schema()::regnamespace AND $2
ORDER BY 1`

// schema lists tables' columns, indexes, constraints and triggers, plus the schema's functions when withFunctions is set.
func schema(t *testing.T, dsn string, tables []string, withFunctions bool) []string {
	t.Helper()
	rows, err := openPool(t, dsn).Query(context.Background(), schemaQuery, tables, withFunctions)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func allTables(t *testing.T, dsn string) []string {
	t.Helper()
	rows, err := openPool(t, dsn).Query(context.Background(),
		"SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename <> $1 ORDER BY 1", MigrationsTable)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestMigrationIsNoOpOnProductSchema(t *testing.T) {
	dsn := emptyDB(t)
	migrateProduct(t, dsn)
	tables := allTables(t, dsn)
	before := schema(t, dsn, tables, true)
	if err := Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	if after := allTables(t, dsn); !slices.Equal(after, tables) {
		t.Fatalf("tables changed:\n before %v\n after  %v", tables, after)
	}
	diff(t, "after the module's migration", schema(t, dsn, tables, true), before)
}

func TestSchemaMatchesProduct(t *testing.T) {
	product := emptyDB(t)
	migrateProduct(t, product)
	module := migratedDB(t)

	want := slices.DeleteFunc(schema(t, product, directoryTables, false), func(s string) bool {
		for _, p := range productOnly {
			if strings.HasPrefix(s, p) {
				return true
			}
		}
		// The product's change-notification triggers are its own; the module reports changes in Go.
		return strings.HasPrefix(s, "trg ") && !strings.Contains(s, "_resource_version ")
	})
	for _, table := range directoryTables {
		if !slices.ContainsFunc(want, func(s string) bool { return strings.HasPrefix(s, "col "+table+" ") }) {
			t.Fatalf("product schema has no table %s", table)
		}
	}
	diff(t, "module schema vs product schema", schema(t, module, directoryTables, false), want)
}

func TestProductMigrationsRunAfterModule(t *testing.T) {
	dsn := migratedDB(t)
	migrateProduct(t, dsn)
}

func diff(t *testing.T, what string, got, want []string) {
	t.Helper()
	var missing, extra []string
	for _, w := range want {
		if !slices.Contains(got, w) {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			extra = append(extra, g)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("%s:\n missing:\n  %s\n unexpected:\n  %s", what, strings.Join(missing, "\n  "), strings.Join(extra, "\n  "))
	}
}
