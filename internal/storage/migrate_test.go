package storage

import "testing"

// `migrate down <n>` above the current version used to run the UP migrations
// the operator was trying to undo.
func TestMigrateDownRefusesATargetAboveTheCurrentVersion(t *testing.T) {
	if _, err := migrateDownTarget(24, true, 30); err == nil {
		t.Fatal("a target above the current version was accepted")
	}
	if _, err := migrateDownTarget(30, true, 24); err != nil {
		t.Fatalf("a target below the current version was refused: %v", err)
	}
	if _, err := migrateDownTarget(30, true, 30); err != nil {
		t.Fatalf("the current version was refused as a target: %v", err)
	}
	// An unmigrated database has no version to compare against.
	if _, err := migrateDownTarget(0, false, 30); err != nil {
		t.Fatalf("an unmigrated database refused a target: %v", err)
	}
}

// Target 0 means "unwind everything", which golang-migrate's Migrate() cannot
// express — it has to run Down().
func TestMigrateDownToZeroUnwindsEverything(t *testing.T) {
	downAllRun, err := migrateDownTarget(30, true, 0)
	if err != nil {
		t.Fatalf("target 0 was refused: %v", err)
	}
	if !downAllRun {
		t.Fatal("target 0 did not select the down-all path")
	}
	if downAllRun, _ := migrateDownTarget(30, true, 24); downAllRun {
		t.Fatal("a non-zero target selected the down-all path")
	}
}
