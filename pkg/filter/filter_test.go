package filter

import (
	"net/url"
	"testing"
	"time"
)

type row struct {
	Name    string
	Enabled bool
	Models  []string
	Ctx     int64
	Created time.Time
}

func schema() Schema[row] {
	return Schema[row]{
		Fields: []Field[row]{
			{Name: "name", Kind: String, Sortable: true, Get: func(r *row) string { return r.Name }},
			{Name: "enabled", Kind: Bool, GetBool: func(r *row) bool { return r.Enabled }},
			{Name: "model_id", Kind: String, Repeat: true, GetMulti: func(r *row) []string { return r.Models }},
			{Name: "tier", Kind: String, Enum: []string{"a", "b"}, Get: func(r *row) string { return r.Name }},
			{Name: "context_window", Kind: Int, Sortable: true, GetInt: func(r *row) int64 { return r.Ctx }},
			{Name: "created", Kind: Time, Sortable: true, GetTime: func(r *row) time.Time { return r.Created }},
		},
		Q:           func(r *row) []string { return []string{r.Name} },
		DefaultSort: "name",
	}
}

func mustParse(t *testing.T, q string) Query[row] {
	t.Helper()
	v, err := url.ParseQuery(q)
	if err != nil {
		t.Fatalf("bad test query %q: %v", q, err)
	}
	pq, err := schema().Parse(v)
	if err != nil {
		t.Fatalf("Parse(%q) unexpected error: %v", q, err)
	}
	return pq
}

func parseErr(t *testing.T, q string) *Error {
	t.Helper()
	v, _ := url.ParseQuery(q)
	_, err := schema().Parse(v)
	if err == nil {
		t.Fatalf("Parse(%q): expected error, got nil", q)
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("Parse(%q): want *Error, got %T", q, err)
	}
	return e
}

var data = []*row{
	{Name: "alpha", Enabled: true, Models: []string{"m1", "m2"}, Ctx: 100, Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	{Name: "beta", Enabled: false, Models: []string{"m2"}, Ctx: 200, Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
	{Name: "gamma", Enabled: true, Models: []string{"m3"}, Ctx: 300, Created: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
}

func names(rows []*row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEqualityAndBool(t *testing.T) {
	got, total := mustParse(t, "enabled=true").Apply(data)
	if want := []string{"alpha", "gamma"}; !eq(names(got), want) || total != 2 {
		t.Fatalf("enabled=true => %v (total %d), want %v", names(got), total, want)
	}
}

func TestRepeatableIN(t *testing.T) {
	got, _ := mustParse(t, "model_id=m2&model_id=m3").Apply(data)
	if want := []string{"alpha", "beta", "gamma"}; !eq(names(got), want) {
		t.Fatalf("model_id IN (m2,m3) => %v, want %v", names(got), want)
	}
	got, _ = mustParse(t, "model_id=m3").Apply(data)
	if want := []string{"gamma"}; !eq(names(got), want) {
		t.Fatalf("model_id=m3 => %v, want %v", names(got), want)
	}
}

func TestNonRepeatableRejectsMulti(t *testing.T) {
	if e := parseErr(t, "tier=a&tier=b"); e.Key != "tier" {
		t.Fatalf("want key tier, got %q", e.Key)
	}
}

func TestEnumValidation(t *testing.T) {
	if e := parseErr(t, "tier=zzz"); e.Key != "tier" {
		t.Fatalf("want enum rejection on tier, got %+v", e)
	}
	mustParse(t, "tier=a") // valid enum value parses
}

func TestUnknownKeyRejected(t *testing.T) {
	if e := parseErr(t, "bogus=1"); e.Key != "bogus" {
		t.Fatalf("want key bogus, got %q", e.Key)
	}
}

func TestIntRange(t *testing.T) {
	got, _ := mustParse(t, "context_window_min=200").Apply(data)
	if want := []string{"beta", "gamma"}; !eq(names(got), want) {
		t.Fatalf("ctx>=200 => %v, want %v", names(got), want)
	}
	got, _ = mustParse(t, "context_window_min=150&context_window_max=250").Apply(data)
	if want := []string{"beta"}; !eq(names(got), want) {
		t.Fatalf("150<=ctx<=250 => %v, want %v", names(got), want)
	}
	if e := parseErr(t, "context_window=200"); e.Key != "context_window" {
		t.Fatalf("bare int field must require _min/_max suffix, got %+v", e)
	}
	if e := parseErr(t, "context_window_min=abc"); e.Key != "context_window_min" {
		t.Fatalf("non-int => error, got %+v", e)
	}
}

func TestTimeRange(t *testing.T) {
	got, _ := mustParse(t, "created_from=2026-02-01T00:00:00Z").Apply(data)
	if want := []string{"beta", "gamma"}; !eq(names(got), want) {
		t.Fatalf("created>=Feb => %v, want %v", names(got), want)
	}
	if e := parseErr(t, "created_from=nope"); e.Key != "created_from" {
		t.Fatalf("bad RFC3339 => error, got %+v", e)
	}
}

func TestFreeText(t *testing.T) {
	got, _ := mustParse(t, "q=AL").Apply(data) // case-insensitive
	if want := []string{"alpha"}; !eq(names(got), want) {
		t.Fatalf("q=AL => %v, want %v", names(got), want)
	}
}

func TestSort(t *testing.T) {
	got, _ := mustParse(t, "sort=-context_window").Apply(data)
	if want := []string{"gamma", "beta", "alpha"}; !eq(names(got), want) {
		t.Fatalf("sort=-context_window => %v, want %v", names(got), want)
	}
	// default sort = name ascending
	got, _ = mustParse(t, "").Apply(data)
	if want := []string{"alpha", "beta", "gamma"}; !eq(names(got), want) {
		t.Fatalf("default sort => %v, want %v", names(got), want)
	}
	if e := parseErr(t, "sort=enabled"); e.Key != "sort" {
		t.Fatalf("non-sortable field must be rejected, got %+v", e)
	}
}

func TestWindowAndTotal(t *testing.T) {
	got, total := mustParse(t, "limit=2").Apply(data)
	if len(got) != 2 || total != 3 {
		t.Fatalf("limit=2 => len %d total %d, want 2/3", len(got), total)
	}
	got, total = mustParse(t, "offset=2").Apply(data)
	if want := []string{"gamma"}; !eq(names(got), want) || total != 3 {
		t.Fatalf("offset=2 => %v total %d, want [gamma]/3", names(got), total)
	}
	got, _ = mustParse(t, "offset=99").Apply(data)
	if len(got) != 0 {
		t.Fatalf("offset past end => %v, want empty", names(got))
	}
	if e := parseErr(t, "limit=-1"); e.Key != "limit" {
		t.Fatalf("negative limit rejected, got %+v", e)
	}
}

func TestComposeAND(t *testing.T) {
	got, _ := mustParse(t, "enabled=true&model_id=m1").Apply(data)
	if want := []string{"alpha"}; !eq(names(got), want) {
		t.Fatalf("enabled=true AND model_id=m1 => %v, want %v", names(got), want)
	}
}

func TestMatchAllMembership(t *testing.T) {
	caps := Schema[row]{Fields: []Field[row]{
		{Name: "cap", Kind: String, Repeat: true, MatchAll: true, GetMulti: func(r *row) []string { return r.Models }},
	}}
	items := []*row{
		{Name: "both", Models: []string{"vision", "tools", "x"}},
		{Name: "one", Models: []string{"vision"}},
	}
	v, _ := url.ParseQuery("cap=vision&cap=tools")
	q, err := caps.Parse(v)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := q.Apply(items)
	if len(got) != 1 || got[0].Name != "both" {
		t.Fatalf("MatchAll cap=vision,tools => %v, want [both]", names(got))
	}
}

func TestLabelSelectors(t *testing.T) {
	s := Schema[row]{
		Fields: []Field[row]{{Name: "name", Kind: String, Sortable: true, Get: func(r *row) string { return r.Name }}},
		Labels: func(r *row) map[string]string {
			if r.Name == "a" {
				return map[string]string{"team": "infra", "env": "prod"}
			}
			return map[string]string{"team": "data"}
		},
	}
	items := []*row{{Name: "a"}, {Name: "b"}}
	v, _ := url.ParseQuery("label=team=infra&label=env=prod")
	q, err := s.Parse(v)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := q.Apply(items)
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("label team=infra,env=prod => %v, want [a]", names(got))
	}
	if _, err := s.Parse(url.Values{"label": {"bogus"}}); err == nil {
		t.Fatal("label without = should 400")
	}
}

func TestEmptyValueIsNoConstraint(t *testing.T) {
	got, total := mustParse(t, "name=").Apply(data)
	if total != 3 || len(got) != 3 {
		t.Fatalf("empty value should not filter, got total %d", total)
	}
}
