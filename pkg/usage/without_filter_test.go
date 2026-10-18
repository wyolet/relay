package usage

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
)

// snakeCase maps an EventQuery field name to its dimension spelling: PolicyID → policy_id, RelayKeyHash → relay_key_hash.
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) && unicode.IsLower(rune(s[i-1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// fullQuery sets every []string filter (scope included) and two tag keys.
func fullQuery() (EventQuery, []string) {
	var q EventQuery
	v := reflect.ValueOf(&q).Elem()
	var fields []string
	for i := range v.NumField() {
		if v.Field(i).Type() == reflect.TypeFor[[]string]() {
			v.Field(i).Set(reflect.ValueOf([]string{"x"}))
			fields = append(fields, v.Type().Field(i).Name)
		}
	}
	q.Tags = map[string][]string{"env": {"prod"}, "leg": {"a"}}
	return q, fields
}

// Every group dimension that has an EventQuery filter field must clear exactly that field; a dimension without one must leave the query untouched. A new filter field named after a dimension fails here until WithoutFilter maps it.
func TestWithoutFilterCoversEveryFilteredDimension(t *testing.T) {
	q, fields := fullQuery()
	for _, dim := range ValidGroupBy {
		got := WithoutFilter(q, dim)
		gv := reflect.ValueOf(got)
		filtered := false
		for _, f := range fields {
			own := snakeCase(f) == dim
			filtered = filtered || own
			if cleared := gv.FieldByName(f).Len() == 0; cleared != own {
				t.Errorf("WithoutFilter(%s): %s cleared = %v, want %v", dim, f, cleared, own)
			}
		}
		if !filtered && !reflect.DeepEqual(got, q) {
			t.Errorf("WithoutFilter(%s) changed a query it has no filter for", dim)
		}
		if !reflect.DeepEqual(got.Tags, q.Tags) {
			t.Errorf("WithoutFilter(%s) touched the tag filter", dim)
		}
	}
	for dim := range dimFilters {
		if !IsValidFacetDim(dim) {
			t.Errorf("dimFilters maps %q, which is not a group dimension", dim)
		}
	}
}

func TestWithoutFilterTags(t *testing.T) {
	q, _ := fullQuery()
	got := WithoutFilter(q, "tags.env")
	if !reflect.DeepEqual(got.Tags, map[string][]string{"leg": {"a"}}) {
		t.Fatalf("tags = %v, want only leg", got.Tags)
	}
	if len(q.Tags) != 2 {
		t.Fatal("the caller's tag map was mutated")
	}
	if got := WithoutFilter(q, "extras.env"); !reflect.DeepEqual(got, q) {
		t.Fatal("extras dimensions have no filter to drop")
	}
	if got := WithoutFilter(q, "tags.missing"); !reflect.DeepEqual(got, q) {
		t.Fatal("an absent tag key must leave the query alone")
	}
}

func TestLabelColumnPairsIDsWithSlugDimensions(t *testing.T) {
	for _, dim := range ValidGroupBy {
		col, ok := LabelColumn(dim)
		if !ok {
			continue
		}
		if !IsValidFacetDim(col) {
			t.Errorf("LabelColumn(%s) = %q, not a group dimension", dim, col)
		}
		if _, nested := LabelColumn(col); nested {
			t.Errorf("LabelColumn(%s) = %q, which is itself an id dimension", dim, col)
		}
	}
	if _, ok := LabelColumn("host_key_id"); ok {
		t.Error("host_key_id has no slug column")
	}
}
