package clickhouse

import (
	"regexp"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/usage"
)

func TestInsertSQLNamesItsColumns(t *testing.T) {
	want := "INSERT INTO usage_events (" + strings.Join(insertColumns, ", ") + ")"
	if insertSQL != want {
		t.Fatalf("insertSQL = %q, want %q", insertSQL, want)
	}
}

func TestInsertValuesMatchColumns(t *testing.T) {
	if got := len(insertValues(usage.Event{})); got != len(insertColumns) {
		t.Fatalf("insertValues returns %d values for %d insertColumns", got, len(insertColumns))
	}
}

func TestInsertColumnsExistInCreateTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range insertColumns {
		if seen[c] {
			t.Errorf("column %q listed twice", c)
		}
		seen[c] = true
		if !regexp.MustCompile(`\n\s+` + c + `\s`).MatchString(createTableSQL) {
			t.Errorf("column %q is not in createTableSQL", c)
		}
	}
}
