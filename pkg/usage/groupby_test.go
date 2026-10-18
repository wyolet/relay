package usage

import (
	"encoding/json"
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

func TestIsValidGroupByAcceptsNone(t *testing.T) {
	if !IsValidGroupBy(GroupByNone) {
		t.Fatal("none rejected")
	}
	if Grouped(GroupByNone) || Grouped("") || !Grouped("model") {
		t.Fatal("Grouped: none and empty must not split rows; a dimension must")
	}
}
