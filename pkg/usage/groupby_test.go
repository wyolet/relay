package usage

import (
	"encoding/json"
	"strings"
	"testing"
)

// An ungrouped series carries no "group" key; group_by=none carries "group": {} so a client can tell the two apart.
func TestTimeSeriesRowGroupJSON(t *testing.T) {
	for _, tc := range []struct {
		groupBy, want string
	}{
		{"", `{"points":null}`},
		{GroupByNone, `{"group":{},"points":null}`},
		{"model", `{"group":{"model":"m"},"points":null}`},
	} {
		got, err := json.Marshal(TimeSeriesRow{Group: GroupLabel(tc.groupBy, "m")})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("group_by %q: %s, want %s", tc.groupBy, got, tc.want)
		}
	}
}

func TestMapGroupKey(t *testing.T) {
	longest := strings.Repeat("k", MaxTagKeyLen)
	for _, tc := range []struct {
		in, column, key string
		ok              bool
	}{
		{"tags.session_id", "tags", "session_id", true},
		{"extras.instance", "extras", "instance", true},
		{"tags.a.b", "tags", "a.b", true},
		{"tags." + longest, "tags", longest, true},
		{"tags." + longest + "k", "", "", false},
		{"tags.", "", "", false},
		{"extras.", "", "", false},
		{"tags", "", "", false},
		{"session_id", "", "", false},
		{"Tags.k", "", "", false},
		{"", "", "", false},
	} {
		column, key, ok := MapGroupKey(tc.in)
		if column != tc.column || key != tc.key || ok != tc.ok {
			t.Errorf("MapGroupKey(%q) = %q, %q, %v; want %q, %q, %v", tc.in, column, key, ok, tc.column, tc.key, tc.ok)
		}
		if got := IsValidGroupBy(tc.in); got != tc.ok {
			t.Errorf("IsValidGroupBy(%q) = %v, want %v", tc.in, got, tc.ok)
		}
	}
}

func TestIsValidGroupBy(t *testing.T) {
	for _, dim := range append([]string{GroupByNone}, ValidGroupBy...) {
		if !IsValidGroupBy(dim) {
			t.Errorf("IsValidGroupBy(%q) = false", dim)
		}
	}
	for _, dim := range []string{"", "nonsense", "MODEL", "none ", "credential_kind"} {
		if IsValidGroupBy(dim) {
			t.Errorf("IsValidGroupBy(%q) = true", dim)
		}
	}
	if Grouped(GroupByNone) || Grouped("") || !Grouped("model") {
		t.Error("Grouped: none and empty must not split rows; a dimension must")
	}
}
