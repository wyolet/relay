package control

import (
	"net/url"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/pkg/filter"
)

func applyQ[T any](t *testing.T, s filter.Schema[T], q string, items []*T) ([]*T, int) {
	t.Helper()
	v, err := url.ParseQuery(q)
	if err != nil {
		t.Fatalf("bad query %q: %v", q, err)
	}
	pq, err := s.Parse(v)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	return pq.Apply(items)
}

func TestPolicyFilter(t *testing.T) {
	on, off := true, false
	items := []*policy.Policy{
		{Meta: meta.Metadata{Name: "a"}, Spec: policy.Spec{Enabled: &on, ModelIDs: []string{"m1", "m2"}, PayloadLoggingEnabled: true}},
		{Meta: meta.Metadata{Name: "b"}, Spec: policy.Spec{Enabled: &off, ModelIDs: []string{"m3"}}},
		{Meta: meta.Metadata{Name: "c"}, Spec: policy.Spec{ModelIDs: []string{"m2"}}}, // nil Enabled => enabled
	}
	got, total := applyQ(t, policyFilter, "enabled=true", items)
	if total != 2 { // a (explicit) + c (nil=true)
		t.Fatalf("enabled=true total=%d, want 2 (got %v)", total, names(got))
	}
	got, _ = applyQ(t, policyFilter, "model_id=m2", items)
	if len(got) != 2 {
		t.Fatalf("model_id=m2 => %v, want a,c", names(got))
	}
	got, _ = applyQ(t, policyFilter, "payload_logging=true", items)
	if len(got) != 1 || got[0].Meta.Name != "a" {
		t.Fatalf("payload_logging=true => %v, want [a]", names(got))
	}
	if _, err := policyFilter.Parse(url.Values{"bogus": {"1"}}); err == nil {
		t.Fatal("unknown key should 400")
	}
}

func TestModelFilter(t *testing.T) {
	on := true
	items := []*model.Model{
		{Meta: meta.Metadata{Name: "big"}, Spec: model.Spec{Enabled: &on, Family: "gpt", ContextWindowTotal: 128000, Tags: []string{"vision"}}},
		{Meta: meta.Metadata{Name: "small"}, Spec: model.Spec{Family: "gpt", ContextWindowTotal: 8000}},
		{Meta: meta.Metadata{Name: "claude"}, Spec: model.Spec{Family: "claude", ContextWindowTotal: 200000, Tags: []string{"vision", "tools"}}},
	}
	got, _ := applyQ(t, modelFilter, "context_window_min=100000", items)
	if want := []string{"big", "claude"}; !eqNames(got, want) {
		t.Fatalf("ctx>=100k => %v, want %v", names(got), want)
	}
	got, _ = applyQ(t, modelFilter, "family=claude", items)
	if len(got) != 1 || got[0].Meta.Name != "claude" {
		t.Fatalf("family=claude => %v", names(got))
	}
	got, _ = applyQ(t, modelFilter, "tag=tools", items)
	if len(got) != 1 || got[0].Meta.Name != "claude" {
		t.Fatalf("tag=tools => %v", names(got))
	}
	got, _ = applyQ(t, modelFilter, "sort=-context_window", items)
	if want := []string{"claude", "big", "small"}; !eqNames(got, want) {
		t.Fatalf("sort=-context_window => %v, want %v", names(got), want)
	}
}

func TestModelFilter_Timestamps(t *testing.T) {
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mar := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	items := []*model.Model{
		{Meta: meta.Metadata{Name: "old", CreatedAt: jan}},
		{Meta: meta.Metadata{Name: "new", CreatedAt: mar}},
		{Meta: meta.Metadata{Name: "unstamped"}}, // zero CreatedAt — excluded by any bound
	}
	got, _ := applyQ(t, modelFilter, "created_from=2026-02-01T00:00:00Z", items)
	if want := []string{"new"}; !eqNames(got, want) {
		t.Fatalf("created_from=Feb => %v, want [new]", names(got))
	}
	got, _ = applyQ(t, modelFilter, "sort=-created", items)
	if want := []string{"new", "old", "unstamped"}; !eqNames(got, want) {
		t.Fatalf("sort=-created => %v, want %v", names(got), want)
	}
}

func TestHostFilter(t *testing.T) {
	items := []*host.Host{
		{Meta: meta.Metadata{Name: "openai"}, Spec: host.Spec{BaseURL: "https://api.openai.com", Policies: []string{"p1", "p2"}}},
		{Meta: meta.Metadata{Name: "bedrock"}, Spec: host.Spec{BaseURL: "https://bedrock.aws", Policies: []string{"p3"}}},
	}
	got, _ := applyQ(t, hostFilter, "policy_id=p2", items)
	if len(got) != 1 || got[0].Meta.Name != "openai" {
		t.Fatalf("policy_id=p2 => %v, want [openai]", names(got))
	}
	got, _ = applyQ(t, hostFilter, "q=aws", items)
	if len(got) != 1 || got[0].Meta.Name != "bedrock" {
		t.Fatalf("q=aws (baseURL) => %v, want [bedrock]", names(got))
	}
}

func TestRelayKeyFilter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	items := []*relaykey.RelayKey{
		{Meta: meta.Metadata{Name: "live"}, Spec: relaykey.Spec{PolicyID: "p1"}},
		{Meta: meta.Metadata{Name: "dead"}, Spec: relaykey.Spec{PolicyID: "p1", RevokedAt: &now}},
		{Meta: meta.Metadata{Name: "other"}, Spec: relaykey.Spec{PolicyID: "p2"}},
	}
	got, _ := applyQ(t, relayKeyFilter, "revoked=true", items)
	if len(got) != 1 || got[0].Meta.Name != "dead" {
		t.Fatalf("revoked=true => %v, want [dead]", names(got))
	}
	got, _ = applyQ(t, relayKeyFilter, "policy_id=p1", items)
	if want := []string{"dead", "live"}; !eqNames(got, want) { // default sort=name
		t.Fatalf("policy_id=p1 => %v, want %v", names(got), want)
	}
}

// --- small local helpers (kept distinct from filter pkg's own test helpers) ---

func names[T any](rows []*T) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		switch v := any(r).(type) {
		case *policy.Policy:
			out[i] = v.Meta.Name
		case *model.Model:
			out[i] = v.Meta.Name
		case *host.Host:
			out[i] = v.Meta.Name
		case *relaykey.RelayKey:
			out[i] = v.Meta.Name
		}
	}
	return out
}

func eqNames[T any](rows []*T, want []string) bool {
	got := names(rows)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
