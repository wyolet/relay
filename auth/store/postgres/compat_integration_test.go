//go:build integration

package postgres

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The product in this repository created the directory tables before the module existed. These tests prove the module's first migration leaves its schema alone, that the product's schema with the module's later migrations on top agrees with the module's own, and that the module reads and writes the rows it has.

var directoryTables = []string{
	"users", "teams", "projects", "service_accounts", "groups", "group_members",
	"roles", "role_bindings", "role_binding_subjects", "identities",
}

// productOnly are the schema entries the product adds to the shared tables and the module deliberately leaves out.
var productOnly = []string{
	"con role_bindings role_bindings_scope_kind_check",
}

// moduleTriggers are the triggers the module creates; every other trigger on the shared tables is the product's.
var moduleTriggers = []string{"_resource_version ", "users_primary_identity "}

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
	migrateModule(t, dsn, 1)
	if after := allTables(t, dsn); !slices.Equal(after, tables) {
		t.Fatalf("tables changed:\n before %v\n after  %v", tables, after)
	}
	diff(t, "after the module's first migration", schema(t, dsn, tables, true), before)
}

func TestSchemaMatchesProduct(t *testing.T) {
	product := emptyDB(t)
	migrateProduct(t, product)
	if err := Migrate(product); err != nil {
		t.Fatal(err)
	}
	module := migratedDB(t)

	want := slices.DeleteFunc(schema(t, product, directoryTables, false), func(s string) bool {
		for _, p := range productOnly {
			if strings.HasPrefix(s, p) {
				return true
			}
		}
		// The product's change-notification triggers are its own; the module reports changes in Go.
		return strings.HasPrefix(s, "trg ") && !slices.ContainsFunc(moduleTriggers, func(m string) bool { return strings.Contains(s, m) })
	})
	got := schema(t, module, directoryTables, false)
	for _, table := range directoryTables {
		for name, s := range map[string][]string{"product": want, "module": got} {
			if !slices.ContainsFunc(s, func(s string) bool { return strings.HasPrefix(s, "col "+table+" ") }) {
				t.Fatalf("%s schema has no table %s", name, table)
			}
		}
	}
	diff(t, "module schema vs product schema", got, want)
	if a, b := mirrorFunction(t, module), mirrorFunction(t, product); a != b {
		t.Fatalf("identity mirror function differs:\n module  %s\n product %s", a, b)
	}
}

// mirrorFunction is the identity mirror's entry in dsn's schema: its name and source digest. The product created bump_resource_version itself, with a source of its own.
func mirrorFunction(t *testing.T, dsn string) string {
	t.Helper()
	for _, s := range schema(t, dsn, nil, true) {
		if strings.HasPrefix(s, "fn mirror_primary_identity ") {
			return s
		}
	}
	t.Fatalf("no mirror_primary_identity function in %s", dsn)
	return ""
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
