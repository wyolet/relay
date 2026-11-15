package main

import (
	"strings"
	"testing"
)

// TestMigrateDownArguments pins the rollback command's argument contract;
// the migration itself is exercised against Postgres in integration.
func TestMigrateDownArguments(t *testing.T) {
	t.Setenv("RELAY_PG_DSN", "")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no target", []string{"down"}, "exactly one target version"},
		{"unknown direction", []string{"sideways"}, "unknown argument"},
		{"target is not a number", []string{"down", "v24"}, "not a schema version"},
		{"no dsn", []string{"down", "24"}, "RELAY_PG_DSN required"},
		{"up with a version", []string{"up", "24"}, "takes no arguments"},
		{"up without a dsn", []string{"up"}, "RELAY_PG_DSN required"},
		{"auth without a direction", []string{"auth"}, "want 'auth down <version>'"},
		{"auth up", []string{"auth", "up"}, "want 'auth down <version>'"},
		{"auth down without a target", []string{"auth", "down"}, "want 'auth down <version>'"},
		{"auth target is not a number", []string{"auth", "down", "v3"}, "not a schema version"},
		{"auth below the module's first version", []string{"auth", "down", "0"}, "lowest target is 1"},
		{"auth without a dsn", []string{"auth", "down", "3"}, "RELAY_PG_DSN required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runMigrate(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runMigrate(%v) = %v, want an error containing %q", tc.args, err, tc.want)
			}
		})
	}
	// `force` clears a dirty schema left by a failed migration.
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"force without a version", []string{"force"}, "exactly one version"},
		{"force version is not a number", []string{"force", "v24"}, "not a schema version"},
		{"force without a dsn", []string{"force", "24"}, "RELAY_PG_DSN required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runMigrate(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runMigrate(%v) = %v, want an error containing %q", tc.args, err, tc.want)
			}
		})
	}
	// A bare `migrate` keeps its old meaning: boot runs the up-migrations.
	if err := runMigrate(nil); err != nil {
		t.Fatalf("runMigrate(nil) = %v, want nil", err)
	}
}
