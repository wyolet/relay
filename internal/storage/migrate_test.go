package storage

import (
	"math"
	"testing"
)

// The bound is checked before any database or migration source is touched, so the empty DSN is never dialed.
func TestForceVersionRejectsAVersionAboveInt32(t *testing.T) {
	err := ForceVersion("", math.MaxInt32+1)
	if err == nil || err.Error() != "storage: force 2147483648: version exceeds 2147483647" {
		t.Fatalf("err = %v, want the version bound error", err)
	}
}

// `migrate down <n>` above the current version must be refused, not run as the up-migrations the operator is trying to undo; target 0 means "unwind everything", which golang-migrate's Migrate() cannot express, so it selects Down().
func TestMigrateDownTarget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		current    uint
		hasVersion bool
		target     uint
		downAll    bool
		err        string
	}{
		{"below the current version", 30, true, 24, false, ""},
		{"the current version", 30, true, 30, false, ""},
		{"one above the current version", 30, true, 31, false, "storage: migrate down to 31: the schema is at 30 — a higher target would migrate UP; pass a version at or below the current one"},
		{"zero unwinds everything", 30, true, 0, true, ""},
		{"an unmigrated database has no version to compare against", 0, false, 30, false, ""},
		{"zero on an unmigrated database", 0, false, 0, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downAllRun, err := migrateDownTarget(tc.current, tc.hasVersion, tc.target)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if downAllRun != tc.downAll || msg != tc.err {
				t.Errorf("migrateDownTarget = %v, %q; want %v, %q", downAllRun, msg, tc.downAll, tc.err)
			}
		})
	}
}
