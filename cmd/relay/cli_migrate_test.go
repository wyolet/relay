package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/apply"
)

// A conflict means the row changed between plan and write and was skipped,
// so the tree is not applied. TestApplyConflictExitsOne pins that CI sees a
// non-zero exit rather than a clean run.
func TestApplyConflictExitsOne(t *testing.T) {
	srv := planServer(t, http.StatusOK, []apply.Entry{
		{Kind: "Team", Name: "platform", Action: apply.ActionConflict},
	})
	var err error
	captureStdout(t, func() {
		err = runApply([]string{"-f", manifestDir(t), "--url", srv.URL})
	})
	if got := exitCode(t, err); got != exitDrift {
		t.Fatalf("exit %d, want %d", got, exitDrift)
	}

	// --force overrides operator edits, not a mid-apply change.
	captureStdout(t, func() {
		err = runApply([]string{"-f", manifestDir(t), "--force", "--url", srv.URL})
	})
	if got := exitCode(t, err); got != exitDrift {
		t.Fatalf("--force exit %d, want %d", got, exitDrift)
	}
}

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
		{"unknown direction", []string{"up"}, "unknown argument"},
		{"target is not a number", []string{"down", "v24"}, "not a schema version"},
		{"no dsn", []string{"down", "24"}, "RELAY_PG_DSN required"},
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
