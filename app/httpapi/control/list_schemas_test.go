package control

import (
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/filter"
	"github.com/wyolet/relay/pkg/meta"
)

// filterCase is a query and either the names it keeps, in the order the list returns them, or the refusal it gets.
type filterCase struct {
	q       string
	want    []string
	total   int // when a limit cuts the page short of the matches
	refused string
}

func checkFilter[T any](t *testing.T, s filter.Schema[T], items []*T, cases ...filterCase) {
	t.Helper()
	for _, c := range cases {
		v, err := url.ParseQuery(c.q)
		if err != nil {
			t.Fatalf("bad query %q: %v", c.q, err)
		}
		pq, err := s.Parse(v)
		if c.refused != "" {
			if err == nil || err.Error() != c.refused {
				t.Errorf("%s: Parse = %v, want %q", c.q, err, c.refused)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Parse: %v", c.q, err)
			continue
		}
		rows, total := pq.Apply(items)
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = reflect.ValueOf(r).Elem().FieldByName("Meta").Interface().(meta.Metadata).Name
		}
		if len(got) == 0 {
			got = nil
		}
		if diff := cmp.Diff(c.want, got); diff != "" || total != max(c.total, len(c.want)) {
			t.Errorf("%s: total %d, names (-want +got):\n%s", c.q, total, diff)
		}
	}
}

func named(name string) meta.Metadata { return meta.Metadata{Name: name} }

func ownedRow(name string, kind meta.OwnerKind, id string) meta.Metadata {
	return meta.Metadata{Name: name, Owner: meta.Owner{Kind: kind, ID: id}}
}

func labelled(name, k, v string) meta.Metadata {
	return meta.Metadata{Name: name, Labels: map[string]string{k: v}}
}

func TestPolicyFilter(t *testing.T) {
	on, off := true, false
	jan, mar := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	a := &policy.Policy{Meta: labelled("a", "team", "infra"), Spec: policy.Spec{Enabled: &on, PoolIDs: []string{"p1", "p2"}, RateLimits: []policy.RateLimitLink{{RateLimitID: "rl1"}}}}
	a.Meta.CreatedAt, a.Meta.DisplayName = jan, "Alpha tier"
	b := &policy.Policy{Meta: labelled("b", "team", "data"), Spec: policy.Spec{Enabled: &off, PoolIDs: []string{"p3"}, Budgets: []policy.BudgetLink{{BudgetID: "b1"}}}}
	b.Meta.CreatedAt = mar
	c := &policy.Policy{Meta: named("c"), Spec: policy.Spec{PoolIDs: []string{"p2", "k1"}}}
	checkFilter(t, policyFilter, []*policy.Policy{c, a, b},
		filterCase{q: "", want: []string{"a", "b", "c"}},
		// A policy with no enabled flag is enabled.
		filterCase{q: "enabled=true", want: []string{"a", "c"}},
		filterCase{q: "enabled=false", want: []string{"b"}},
		filterCase{q: "pool_id=p2", want: []string{"a", "c"}},
		filterCase{q: "pool_id=k1&pool_id=p3", want: []string{"b", "c"}},
		filterCase{q: "rate_limit_id=rl1", want: []string{"a"}},
		filterCase{q: "budget_id=b1", want: []string{"b"}},
		filterCase{q: "label=team=infra", want: []string{"a"}},
		filterCase{q: "q=alpha", want: []string{"a"}},
		// A row with no creation time passes no bound and sorts last either way.
		filterCase{q: "created_from=2026-02-01T00:00:00Z", want: []string{"b"}},
		filterCase{q: "created_to=2026-02-01T00:00:00Z", want: []string{"a"}},
		filterCase{q: "sort=-created", want: []string{"b", "a", "c"}},
		filterCase{q: "sort=-name&limit=2", want: []string{"c", "b"}, total: 3},
		filterCase{q: "limit=1&offset=1", want: []string{"b"}, total: 3},
		filterCase{q: "enabled=true&pool_id=p1", want: []string{"a"}},
		filterCase{q: "pool_id=nope"},
		filterCase{q: "bogus=1", refused: "bogus: unknown filter field"},
		filterCase{q: "enabled=maybe", refused: "enabled: must be true or false"},
		filterCase{q: "sort=pool_id", refused: `sort: cannot sort by "pool_id"`},
		filterCase{q: "annotation=a=b", refused: "annotation: unknown filter field"},
	)
}

func TestModelFilter(t *testing.T) {
	on, off := true, false
	big := &model.Model{Meta: ownedRow("big", meta.OwnerProvider, "prov1"), Spec: model.Spec{Enabled: &on, Family: "gpt", ContextWindowTotal: 128000, MaxOutputTokens: 4096,
		Tags: []string{"vision"}, License: "proprietary", ReleaseDate: "2025-05-01",
		Capabilities: model.Capabilities{Vision: true, Tools: true, Chat: true}, Modalities: model.Modalities{Input: []string{"text", "image"}}}}
	small := &model.Model{Meta: ownedRow("small", meta.OwnerProvider, "prov1"), Spec: model.Spec{Enabled: &off, Family: "gpt", ContextWindowTotal: 8000,
		DeprecationDate: "2026-01-15", Capabilities: model.Capabilities{Vision: true, Chat: true}, Modalities: model.Modalities{Input: []string{"text"}}}}
	claude := &model.Model{Meta: ownedRow("claude", meta.OwnerProvider, "prov2"), Spec: model.Spec{Family: "claude", ProductLine: "opus", ContextWindowTotal: 200000,
		MaxOutputTokens: 64000, Tags: []string{"vision", "tools"}, ReleaseDate: "2026-02-01T00:00:00Z"}}
	haiku := &model.Model{Meta: ownedRow("haiku", meta.OwnerProvider, "prov2"), Spec: model.Spec{Family: "claude", ProductLine: "haiku", ReleaseDate: "soon"}}
	haiku.Meta.CreatedAt = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	checkFilter(t, modelFilter, []*model.Model{big, small, claude, haiku},
		filterCase{q: "context_window_min=100000", want: []string{"big", "claude"}},
		filterCase{q: "context_window_max=8000", want: []string{"haiku", "small"}},
		filterCase{q: "max_output_tokens_min=4096", want: []string{"big", "claude"}},
		filterCase{q: "family=claude", want: []string{"claude", "haiku"}},
		filterCase{q: "family=claude&family=gpt&enabled=false", want: []string{"small"}},
		filterCase{q: "deprecated=true", want: []string{"small"}},
		filterCase{q: "license=proprietary", want: []string{"big"}},
		filterCase{q: "provider_id=prov2", want: []string{"claude", "haiku"}},
		filterCase{q: "tag=tools", want: []string{"claude"}},
		filterCase{q: "product_line=opus&product_line=haiku", want: []string{"claude", "haiku"}},
		// q reaches the product line, not only the name.
		filterCase{q: "q=haik", want: []string{"haiku"}},
		// Capabilities must all be present; modalities match any.
		filterCase{q: "capability=vision&capability=tools", want: []string{"big"}},
		filterCase{q: "capability=vision", want: []string{"big", "small"}},
		filterCase{q: "modality=image", want: []string{"big"}},
		// A catalog date is YYYY-MM-DD or RFC 3339; one that is neither passes no bound.
		filterCase{q: "released_from=2026-01-01T00:00:00Z", want: []string{"claude"}},
		filterCase{q: "released_to=2026-01-01T00:00:00Z", want: []string{"big"}},
		filterCase{q: "deprecated_date_from=2026-01-01T00:00:00Z", want: []string{"small"}},
		filterCase{q: "created_from=2026-02-01T00:00:00Z", want: []string{"haiku"}},
		filterCase{q: "sort=-context_window", want: []string{"claude", "big", "small", "haiku"}},
		filterCase{q: "sort=product_line", want: []string{"big", "small", "haiku", "claude"}},
		filterCase{q: "sort=-released", want: []string{"claude", "big", "small", "haiku"}},
		filterCase{q: "capability=telepathy", refused: `capability: invalid value "telepathy" (allowed: chat, embeddings, streaming, tools, parallelTools, vision, audio, promptCache, reasoning, jsonMode, structuredOutputs, batch)`},
		filterCase{q: "context_window_min=lots", refused: "context_window_min: must be an integer"},
		filterCase{q: "released_from=yesterday", refused: "released_from: must be an RFC3339 timestamp"},
	)
}

func TestCatalogKindFilters(t *testing.T) {
	off := false
	checkFilter(t, hostFilter, []*host.Host{
		{Meta: named("openai"), Spec: host.Spec{BaseURL: "https://api.openai.com"}},
		{Meta: named("bedrock"), Spec: host.Spec{BaseURL: "https://bedrock.aws", Enabled: &off}},
	},
		filterCase{q: "q=aws", want: []string{"bedrock"}},
		filterCase{q: "enabled=true", want: []string{"openai"}},
		filterCase{q: "policy_id=p1", refused: "policy_id: unknown filter field"},
	)
	checkFilter(t, providerFilter, []*provider.Provider{{Meta: named("acme")}, {Meta: named("zeta"), Spec: provider.Spec{Enabled: &off}}},
		filterCase{q: "enabled=false", want: []string{"zeta"}},
		filterCase{q: "sort=-name", want: []string{"zeta", "acme"}},
	)
	checkFilter(t, pricingFilter, []*pricing.Pricing{
		{Meta: named("flat"), Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{"m1"}, Rates: []pricing.Rate{{Meter: "tokens.input", Unit: pricing.UnitPerMillion}}}},
		{Meta: named("tiered"), Spec: pricing.Spec{Currency: "EUR", TargetModelIDs: []string{"m2"}, Rates: []pricing.Rate{{Meter: "tokens.input", AboveTokens: 1000}, {Meter: "tokens.output", Unit: pricing.UnitPerUnit}}}},
	},
		filterCase{q: "has_tiers=true", want: []string{"tiered"}},
		filterCase{q: "has_tiers=false", want: []string{"flat"}},
		filterCase{q: "target_model_id=m1", want: []string{"flat"}},
		filterCase{q: "currency=EUR", want: []string{"tiered"}},
		filterCase{q: "meter=tokens.input", want: []string{"flat", "tiered"}},
		filterCase{q: "meter=tokens.output", want: []string{"tiered"}},
		filterCase{q: "unit=per_unit", want: []string{"tiered"}},
	)
	checkFilter(t, bindingFilter, []*binding.Binding{
		{Meta: named("m1-at-h1"), Spec: binding.Spec{ModelID: "m1", HostID: "h1", PricingID: "pr1", Adapter: "openai"}},
		{Meta: named("m1-at-h2"), Spec: binding.Spec{ModelID: "m1", HostID: "h2", Adapter: "anthropic", Enabled: &off}},
	},
		filterCase{q: "model_id=m1", want: []string{"m1-at-h1", "m1-at-h2"}},
		filterCase{q: "host_id=h2", want: []string{"m1-at-h2"}},
		filterCase{q: "pricing_id=pr1", want: []string{"m1-at-h1"}},
		filterCase{q: "adapter=anthropic", want: []string{"m1-at-h2"}},
		filterCase{q: "enabled=true", want: []string{"m1-at-h1"}},
	)
}

func TestCredentialFilters(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	sa := key.Principal{Kind: key.PrincipalServiceAccount, ID: "sa-1"}
	checkFilter(t, keyFilter, []*key.Key{
		{Meta: named("indexer"), Spec: key.Spec{Principal: sa, AccountID: "a1", Prefix: "sk-wr-ix", PassthroughAllowed: true}},
		{Meta: named("expired"), Spec: key.Spec{Principal: sa, ExpiresAt: &past, PayloadLoggingEnabled: true}},
		{Meta: named("personal"), Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: "user-1"}, ExpiresAt: &future}},
		{Meta: named("dead"), Spec: key.Spec{RevokedAt: &past}},
	},
		filterCase{q: "revoked=true", want: []string{"dead"}},
		filterCase{q: "principal_kind=serviceaccount&principal_id=sa-1", want: []string{"expired", "indexer"}},
		filterCase{q: "principal_kind=user", want: []string{"personal"}},
		filterCase{q: "principal_kind=user&principal_kind=serviceaccount", want: []string{"expired", "indexer", "personal"}},
		// A key that expires later, or never, is not expired.
		filterCase{q: "expired=true", want: []string{"expired"}},
		filterCase{q: "expired=false", want: []string{"dead", "indexer", "personal"}},
		filterCase{q: "account_id=a1", want: []string{"indexer"}},
		filterCase{q: "prefix=sk-wr-ix", want: []string{"indexer"}},
		filterCase{q: "passthrough=true", want: []string{"indexer"}},
		filterCase{q: "payload_logging=true", want: []string{"expired"}},
		filterCase{q: "principal_kind=robot", refused: `principal_kind: invalid value "robot" (allowed: serviceaccount, user)`},
		filterCase{q: "policy_id=p1", refused: "policy_id: unknown filter field"},
	)
	checkFilter(t, hostKeyFilter, []*hostkey.HostKey{
		{Meta: named("envkey"), Spec: hostkey.Spec{HostID: "h1", AccountID: "a1", DefaultTier: "tier-2", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv}}},
		{Meta: named("storedkey"), Spec: hostkey.Spec{HostID: "h2", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}}},
	},
		filterCase{q: "account_id=a1", want: []string{"envkey"}},
		filterCase{q: "value_kind=stored", want: []string{"storedkey"}},
		filterCase{q: "host_id=h1&host_id=h2", want: []string{"envkey", "storedkey"}},
		filterCase{q: "default_tier=tier-2", want: []string{"envkey"}},
		filterCase{q: "value_kind=bogus", refused: `value_kind: invalid value "bogus" (allowed: env, stored)`},
	)
}

func TestTenancyFilters(t *testing.T) {
	off := false
	checkFilter(t, teamFilter, []*team.Team{{Meta: labelled("platform", "env", "prod")}, {Meta: named("research"), Spec: team.Spec{Enabled: &off}}},
		filterCase{q: "enabled=true", want: []string{"platform"}},
		filterCase{q: "label=env%3Dprod", want: []string{"platform"}},
		filterCase{q: "label=env%3Ddev"},
		filterCase{q: "annotation=a=b", refused: "annotation: unknown filter field"},
	)
	checkFilter(t, projectFilter, []*project.Project{
		{Meta: labelled("ml-search", "env", "prod"), Spec: project.Spec{TeamID: "t1"}},
		{Meta: named("ranking"), Spec: project.Spec{TeamID: "t1", Enabled: &off}},
		{Meta: named("lab"), Spec: project.Spec{TeamID: "t2"}},
	},
		filterCase{q: "team_id=t1&enabled=true", want: []string{"ml-search"}},
		filterCase{q: "team_id=t1&team_id=t2", want: []string{"lab", "ml-search", "ranking"}},
		filterCase{q: "label=env%3Dprod", want: []string{"ml-search"}},
		filterCase{q: "bogus=1", refused: "bogus: unknown filter field"},
	)
	checkFilter(t, serviceAccountFilter, []*serviceaccount.ServiceAccount{
		{Meta: labelled("indexer", "env", "prod"), Spec: serviceaccount.Spec{ProjectID: "p1"}},
		{Meta: named("ranker"), Spec: serviceaccount.Spec{ProjectID: "p1", Enabled: &off}},
		{Meta: named("lab-bot"), Spec: serviceaccount.Spec{ProjectID: "p2"}},
	},
		filterCase{q: "project_id=p1&enabled=true", want: []string{"indexer"}},
		filterCase{q: "project_id=p2", want: []string{"lab-bot"}},
		filterCase{q: "label=env%3Dprod", want: []string{"indexer"}},
	)
	retired := &group.Group{Meta: named("retired"), Spec: group.Spec{Enabled: &off}}
	retired.Meta.Description = "former data team"
	checkFilter(t, groupFilter, []*group.Group{{Meta: labelled("data-science", "env", "prod")}, retired},
		filterCase{q: "enabled=true", want: []string{"data-science"}},
		filterCase{q: "q=former", want: []string{"retired"}},
		filterCase{q: "bogus=1", refused: "bogus: unknown filter field"},
	)
	checkFilter(t, roleFilter, []*role.Role{{Meta: named("viewer")}, {Meta: named("legacy"), Spec: role.Spec{Enabled: &off}}},
		filterCase{q: "enabled=true", want: []string{"viewer"}},
		filterCase{q: "q=leg", want: []string{"legacy"}},
	)
}

// The two binding lists answer "who has access here".
func TestBindingFilters(t *testing.T) {
	off := false
	user := func(id string) rolebinding.Subject { return rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: id} }
	ml := rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "ml"}
	checkFilter(t, roleBindingFilter, []*rolebinding.RoleBinding{
		{Meta: named("alice-team"), Spec: rolebinding.Spec{RoleID: "r-admin", Scope: meta.Owner{Kind: meta.OwnerTeam, ID: "t1"}, Subjects: []rolebinding.Subject{user("u1")}}},
		{Meta: named("bob-project"), Spec: rolebinding.Spec{RoleID: "r-dev", Scope: meta.Owner{Kind: meta.OwnerProject, ID: "p1"}, Subjects: []rolebinding.Subject{user("u2"), ml}}},
		{Meta: named("ml-global"), Spec: rolebinding.Spec{RoleID: "r-dev", Enabled: &off, Scope: meta.Owner{Kind: meta.OwnerSystem}, Subjects: []rolebinding.Subject{ml}}},
	},
		filterCase{q: "subject=user%3Au2", want: []string{"bob-project"}},
		filterCase{q: "subject=group%3Aml", want: []string{"bob-project", "ml-global"}},
		filterCase{q: "subject=user%3Au1&subject=group%3Aml", want: []string{"alice-team", "bob-project", "ml-global"}},
		filterCase{q: "scope_kind=team", want: []string{"alice-team"}},
		filterCase{q: "scope_kind=system", want: []string{"ml-global"}},
		filterCase{q: "scope_kind=project&scope_id=p1", want: []string{"bob-project"}},
		filterCase{q: "scope_kind=team&scope_id=p1"},
		filterCase{q: "scope_id=t1", want: []string{"alice-team"}},
		filterCase{q: "role_id=r-dev", want: []string{"bob-project", "ml-global"}},
		filterCase{q: "role_id=r-dev&enabled=true", want: []string{"bob-project"}},
		filterCase{q: "scope_kind=user", refused: `scope_kind: invalid value "user" (allowed: system, team, project)`},
		filterCase{q: "bogus=1", refused: "bogus: unknown filter field"},
	)
	checkFilter(t, policyBindingFilter, []*policybinding.PolicyBinding{
		{Meta: ownedRow("search-default", meta.OwnerProject, "p1"), Spec: policybinding.Spec{PolicyID: "pol1", AccountIDs: []string{"a-project"}}},
		{Meta: ownedRow("indexer-override", meta.OwnerProject, "p1"), Spec: policybinding.Spec{PolicyID: "pol2", AccountIDs: []string{"a-sa1"}}},
		{Meta: ownedRow("lab-default", meta.OwnerProject, "p2"), Spec: policybinding.Spec{PolicyID: "pol1", AccountIDs: []string{"a-lab"}, Enabled: &off}},
		{Meta: ownedRow("tiers", meta.OwnerSystem, ""), Spec: policybinding.Spec{PolicyID: "pol3", AccountIDs: []string{"a-org", "a-sa1-x"}}},
	},
		filterCase{q: "owner_id=p1", want: []string{"indexer-override", "search-default"}},
		filterCase{q: "owner=system", want: []string{"tiers"}},
		filterCase{q: "policy_id=pol1", want: []string{"lab-default", "search-default"}},
		filterCase{q: "policy_id=pol1&enabled=true", want: []string{"search-default"}},
		filterCase{q: "owner_id=p1&policy_id=pol2", want: []string{"indexer-override"}},
		// An account id matches whole, never as a prefix of another.
		filterCase{q: "account_id=a-sa1", want: []string{"indexer-override"}},
		filterCase{q: "account_id=a-org", want: []string{"tiers"}},
		filterCase{q: "scope_kind=team", refused: "scope_kind: unknown filter field"},
	)
}

func TestAccountPoolAndBudgetFilters(t *testing.T) {
	alice := &account.Account{Meta: ownedRow("alice", meta.OwnerUser, "u1"), Spec: account.Spec{Kind: account.KindPersonal, Holder: &account.Holder{ID: "u1"}}}
	checkFilter(t, accountFilter, []*account.Account{
		alice,
		{Meta: ownedRow("acme-openai", meta.OwnerTeam, "t1"), Spec: account.Spec{Kind: account.KindProvider, HostID: "h1", OrgID: "org-acme"}},
		{Meta: ownedRow("left", meta.OwnerProject, "p1"), Spec: account.Spec{Kind: account.KindMember, Holder: &account.Holder{ID: "u2"}, Closed: true}},
	},
		filterCase{q: "kind=provider", want: []string{"acme-openai"}},
		filterCase{q: "kind=personal&kind=member", want: []string{"alice", "left"}},
		filterCase{q: "holder_id=u2", want: []string{"left"}},
		filterCase{q: "closed=true", want: []string{"left"}},
		filterCase{q: "closed=false", want: []string{"acme-openai", "alice"}},
		filterCase{q: "host_id=h1", want: []string{"acme-openai"}},
		filterCase{q: "org_id=org-acme", want: []string{"acme-openai"}},
		filterCase{q: "q=org-acme", want: []string{"acme-openai"}},
		filterCase{q: "kind=shared", refused: `kind: invalid value "shared" (allowed: team, project, personal, member, service, provider)`},
	)
	checkFilter(t, poolFilter, []*pool.Pool{
		{Meta: named("fast"), Spec: pool.Spec{HostID: "h1", CredentialIDs: []string{"k1", "k2"}, Selection: "round-robin"}},
		{Meta: named("cheap"), Spec: pool.Spec{HostID: "h2", AccountIDs: []string{"a1"}, Selection: "prioritized"}},
	},
		filterCase{q: "host_id=h1", want: []string{"fast"}},
		filterCase{q: "credential_id=k2", want: []string{"fast"}},
		filterCase{q: "account_id=a1", want: []string{"cheap"}},
		filterCase{q: "selection=prioritized", want: []string{"cheap"}},
		filterCase{q: "selection=random", refused: `selection: invalid value "random" (allowed: prioritized, round-robin, least-recently-used)`},
	)
	checkFilter(t, budgetFilter, []*budget.Budget{
		{Meta: named("monthly-hard"), Spec: budget.Spec{Period: budget.PeriodMonth, Mode: budget.ModeHard}},
		{Meta: named("daily-alert"), Spec: budget.Spec{Period: budget.PeriodDay, Mode: budget.ModeAlert}},
	},
		filterCase{q: "period=month", want: []string{"monthly-hard"}},
		filterCase{q: "mode=alert", want: []string{"daily-alert"}},
		filterCase{q: "period=year", refused: `period: invalid value "year" (allowed: day, week, month)`},
	)
}

// Every kind a person owns filters on its owner the same way.
func TestOwnerFilters(t *testing.T) {
	rows := []meta.Metadata{ownedRow("in-team", meta.OwnerTeam, "t1"), ownedRow("in-project", meta.OwnerProject, "p1"), ownedRow("personal", meta.OwnerUser, "u1")}
	cases := []filterCase{
		{q: "owner=team", want: []string{"in-team"}},
		{q: "owner_id=p1", want: []string{"in-project"}},
		{q: "owner_id=t1&owner_id=u1", want: []string{"in-team", "personal"}},
		{q: "owner=team&owner_id=p1"},
	}
	check := func(name string, run func(t *testing.T)) { t.Run(name, run) }
	check("policies", func(t *testing.T) {
		checkFilter(t, policyFilter, []*policy.Policy{{Meta: rows[0]}, {Meta: rows[1]}, {Meta: rows[2]}}, cases...)
	})
	check("pools", func(t *testing.T) {
		checkFilter(t, poolFilter, []*pool.Pool{{Meta: rows[0]}, {Meta: rows[1]}, {Meta: rows[2]}}, cases...)
	})
	check("budgets", func(t *testing.T) {
		checkFilter(t, budgetFilter, []*budget.Budget{{Meta: rows[0]}, {Meta: rows[1]}, {Meta: rows[2]}}, cases...)
	})
	check("accounts", func(t *testing.T) {
		checkFilter(t, accountFilter, []*account.Account{{Meta: rows[0]}, {Meta: rows[1]}, {Meta: rows[2]}}, cases...)
	})
	check("rate-limits", func(t *testing.T) {
		checkFilter(t, rateLimitFilter, []*ratelimit.RateLimit{{Meta: rows[0]}, {Meta: rows[1]}, {Meta: rows[2]}}, cases...)
	})
	check("keys", func(t *testing.T) {
		checkFilter(t, keyFilter, []*key.Key{{Meta: rows[0]}, {Meta: rows[1]}, {Meta: rows[2]}}, cases...)
	})
	check("host-keys", func(t *testing.T) {
		checkFilter(t, hostKeyFilter, []*hostkey.HostKey{{Meta: rows[0]}, {Meta: rows[1]}, {Meta: rows[2]}}, cases...)
	})
	check("service-accounts", func(t *testing.T) {
		checkFilter(t, serviceAccountFilter, []*serviceaccount.ServiceAccount{{Meta: rows[0]}, {Meta: rows[1]}, {Meta: rows[2]}}, cases...)
	})
}
