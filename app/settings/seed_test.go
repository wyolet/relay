package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSectionFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// usage-logging is a registered section.
	write("usage-logging.yaml", "backend: clickhouse\nclickhouse:\n  retentionDays: 14\n")
	write("notes.txt", "ignored") // non-yaml skipped

	got, err := loadSectionFiles(dir)
	if err != nil {
		t.Fatalf("loadSectionFiles: %v", err)
	}
	raw, ok := got["usage-logging"]
	if !ok {
		t.Fatalf("usage-logging not loaded: %v", got)
	}
	// The JSON must Decode+validate through the section.
	v, err := UsageLoggingDecodeForTest(raw)
	if err != nil {
		t.Fatalf("decode seeded value: %v", err)
	}
	if v.Backend != "clickhouse" || v.CH.RetentionDays != 14 {
		t.Fatalf("seeded value mismatch: %+v", v)
	}
}

func TestLoadSectionFiles_UnknownSection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nope-section.yaml"), []byte("a: 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSectionFiles(dir); err == nil {
		t.Fatal("want error for unknown section")
	}
}

func TestLoadSectionFiles_MissingDirIsEmpty(t *testing.T) {
	got, err := loadSectionFiles(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(got) != 0 {
		t.Fatalf("missing dir: err=%v got=%v", err, got)
	}
}

// UsageLoggingDecodeForTest decodes raw JSON into the typed value via the
// registered section, asserting the seed path produces section-valid JSON.
func UsageLoggingDecodeForTest(raw json.RawMessage) (*UsageLogging, error) {
	sec, _ := Lookup(SectionUsageLogging)
	v, err := sec.Decode(raw)
	if err != nil {
		return nil, err
	}
	return v.(*UsageLogging), nil
}
