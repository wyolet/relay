package catalogvalidate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/model"
)

var (
	system    = manifest.WireOwner{Kind: "system"}
	operator  = manifest.WireOwner{Kind: "user"}
	alice     = manifest.WireOwner{Kind: "user", Name: "alice"}
	inSearch  = manifest.WireOwner{Kind: "project", Name: "ml-search"}
	inTeam    = manifest.WireOwner{Kind: "team", Name: "platform"}
	atOpenAI  = manifest.WireOwner{Kind: "host", Name: "openai-host"}
	envSecret = manifest.HostKeyValueFrom{Kind: "env", Env: "X"}
)

// fixture is a clean graph the cases mutate: one provider, host and model, the
// binding that serves it, and a team with two projects.
func fixture() []manifest.Document {
	return []manifest.Document{
		{Provider: &manifest.ProviderDTO{Metadata: manifest.WireMeta{Name: "openai", Owner: system}}},
		{Host: &manifest.HostDTO{Metadata: manifest.WireMeta{Name: "openai-host", Owner: system}, Spec: manifest.HostSpec{BaseURL: "https://api.openai.com"}}},
		{Model: &manifest.ModelDTO{
			Metadata: manifest.WireMeta{Name: "gpt-x", Owner: manifest.WireOwner{Kind: "provider", Name: "openai"}},
			Spec:     manifest.ModelSpec{Snapshots: []model.Snapshot{{Name: "gpt-x", OriginalName: "gpt-x"}}, Pointer: "gpt-x"},
		}},
		{HostBinding: &manifest.HostBindingDTO{
			Metadata: manifest.WireMeta{Name: "gpt-x-openai-host", Owner: system},
			Spec:     manifest.HostBindingSpec{Model: "gpt-x", Host: "openai-host", Adapter: "openai"},
		}},
		team("platform"),
		project("ml-search", "platform"),
		project("other", "platform"),
	}
}

func team(name string) manifest.Document {
	return manifest.Document{Team: &manifest.TeamDTO{Metadata: manifest.WireMeta{Name: name, Owner: system}}}
}

func project(name, teamName string) manifest.Document {
	return manifest.Document{Project: &manifest.ProjectDTO{
		Metadata: manifest.WireMeta{Name: name, Owner: manifest.WireOwner{Kind: "team", Name: teamName}},
		Spec:     manifest.ProjectSpec{Team: teamName},
	}}
}

func host(name string) manifest.Document {
	return manifest.Document{Host: &manifest.HostDTO{Metadata: manifest.WireMeta{Name: name, Owner: system}}}
}

func serviceAccount(name, projectName string) manifest.Document {
	return manifest.Document{ServiceAccount: &manifest.ServiceAccountDTO{
		Metadata: manifest.WireMeta{Name: name, Owner: manifest.WireOwner{Kind: "project", Name: projectName}},
		Spec:     manifest.ServiceAccountSpec{Project: projectName},
	}}
}

func key(name, principalKind, principalName string) manifest.Document {
	return manifest.Document{Key: &manifest.KeyDTO{
		Metadata: manifest.WireMeta{Name: name},
		Spec:     manifest.KeySpec{Principal: manifest.PrincipalDTO{Kind: principalKind, Name: principalName}},
	}}
}

func hostKey(name, hostName, accountName string, owner manifest.WireOwner) manifest.Document {
	return manifest.Document{HostKey: &manifest.HostKeyDTO{
		Metadata: manifest.WireMeta{Name: name, Owner: owner},
		Spec:     manifest.HostKeySpec{HostID: hostName, Account: accountName, ValueFrom: envSecret},
	}}
}

func account(name, hostName string, owner manifest.WireOwner) manifest.Document {
	return manifest.Document{Account: &manifest.AccountDTO{Metadata: manifest.WireMeta{Name: name, Owner: owner}, Spec: manifest.AccountSpec{Host: hostName}}}
}

func pool(name, hostName string, owner manifest.WireOwner, credentials ...string) manifest.Document {
	return manifest.Document{Pool: &manifest.PoolDTO{
		Metadata: manifest.WireMeta{Name: name, Owner: owner},
		Spec:     manifest.PoolSpec{Host: hostName, Credentials: credentials},
	}}
}

func rateLimit(name string, owner manifest.WireOwner) manifest.Document {
	return manifest.Document{RateLimit: &manifest.RateLimitDTO{Metadata: manifest.WireMeta{Name: name, Owner: owner}}}
}

func budget(name string, owner manifest.WireOwner) manifest.Document {
	return manifest.Document{Budget: &manifest.BudgetDTO{Metadata: manifest.WireMeta{Name: name, Owner: owner}}}
}

func policy(name string, owner manifest.WireOwner, spec manifest.PolicySpec) manifest.Document {
	return manifest.Document{Policy: &manifest.PolicyDTO{Metadata: manifest.WireMeta{Name: name, Owner: owner}, Spec: spec}}
}

func pricing(name string, owner manifest.WireOwner, targets ...string) manifest.Document {
	return manifest.Document{Pricing: &manifest.PricingDTO{
		Metadata: manifest.WireMeta{Name: name, Owner: owner},
		Spec:     manifest.PricingSpec{Currency: "USD", TargetModels: targets},
	}}
}

func policyBinding(name string, owner manifest.WireOwner, pol string, accounts ...string) manifest.Document {
	return manifest.Document{PolicyBinding: &manifest.PolicyBindingDTO{
		Metadata: manifest.WireMeta{Name: name, Owner: owner},
		Spec:     manifest.PolicyBindingSpec{Policy: pol, Accounts: accounts},
	}}
}

func roleBinding(name, roleName string, scope manifest.WireOwner, subjects ...manifest.SubjectDTO) manifest.Document {
	return manifest.Document{RoleBinding: &manifest.RoleBindingDTO{
		Metadata: manifest.WireMeta{Name: name},
		Spec:     manifest.RoleBindingSpec{Role: roleName, Scope: scope, Subjects: subjects},
	}}
}

func rateLimits(ref string, models ...string) manifest.PolicySpec {
	return manifest.PolicySpec{RateLimits: []manifest.PolicyRateLimitDTO{{Ref: ref, Models: models}}}
}

// lines renders issues one per line: severity, kind, source, target and message.
func lines(issues []Issue) []string {
	var out []string
	for _, is := range issues {
		target := ""
		if is.Target != (Ref{}) {
			target = " → " + is.Target.String()
		}
		out = append(out, fmt.Sprintf("%s %s %s%s: %s", is.Severity, is.Kind, is.Source, target, is.Message))
	}
	return out
}

// graphCases are the fixture with documents added and, optionally, edited in place.
var graphCases = []struct {
	name string
	add  []manifest.Document
	edit func(docs []manifest.Document)
}{
	{name: "clean fixture"},

	// Names and the catalog rows.
	{name: "two models with one name", add: []manifest.Document{{Model: &manifest.ModelDTO{
		Metadata: manifest.WireMeta{Name: "gpt-x", Owner: manifest.WireOwner{Kind: "provider", Name: "openai"}},
		Spec:     manifest.ModelSpec{Snapshots: []model.Snapshot{{Name: "gpt-x"}}},
	}}}},
	{name: "two teams with one name, two hosts with one name", add: []manifest.Document{team("platform"), host("openai-host")}},
	{name: "model without snapshots", edit: func(d []manifest.Document) { d[2].Model.Spec.Snapshots = nil }},
	{name: "model of a missing provider, named by id", edit: func(d []manifest.Document) {
		d[2].Model.Metadata.Owner = manifest.WireOwner{Kind: "provider", ID: "ghost"}
	}},
	{name: "model owned by the system", edit: func(d []manifest.Document) { d[2].Model.Metadata.Owner = system }},
	{name: "provider without models, model without a binding", add: []manifest.Document{
		{Provider: &manifest.ProviderDTO{Metadata: manifest.WireMeta{Name: "idle", Owner: system}}},
	}, edit: func(d []manifest.Document) { d[3].HostBinding.Spec.Enabled = new(false) }},

	// Host bindings.
	{name: "binding to a missing host", edit: func(d []manifest.Document) { d[3].HostBinding.Spec.Host = "no-such-host" }},
	{name: "binding of a missing model", edit: func(d []manifest.Document) { d[3].HostBinding.Spec.Model = "no-such-model" }},
	{name: "binding naming neither model nor host", edit: func(d []manifest.Document) { d[3].HostBinding.Spec.Model, d[3].HostBinding.Spec.Host = "", "" }},
	{name: "binding snapshot the model does not declare", edit: func(d []manifest.Document) {
		d[3].HostBinding.Spec.Snapshots = []string{"gpt-x", "gpt-x-typo"}
	}},
	{name: "binding to missing pricing", edit: func(d []manifest.Document) { d[3].HostBinding.Spec.Pricing = "ghost-price" }},
	{name: "binding to present pricing", add: []manifest.Document{pricing("list", atOpenAI, "gpt-x")}, edit: func(d []manifest.Document) {
		d[3].HostBinding.Spec.Pricing = "list"
	}},
	{name: "binding adapter that is no upstream shape", edit: func(d []manifest.Document) { d[3].HostBinding.Spec.Adapter = "canonical" }},

	// Pricing.
	{name: "pricing for a missing model at a missing host", add: []manifest.Document{
		pricing("p1", manifest.WireOwner{Kind: "host", Name: "ghost-host"}, "gpt-x", "ghost-model"),
	}},
	{name: "pricing owned by a host named by id", add: []manifest.Document{pricing("p1", manifest.WireOwner{Kind: "host", ID: "openai-host"}, "gpt-x")}},

	// Tenancy.
	{name: "project of a missing team", add: []manifest.Document{project("stray", "ghost-team")}},
	{name: "project naming no team", add: []manifest.Document{project("stray", "")}},
	{name: "rows owned by a missing project", add: []manifest.Document{
		policy("pol", manifest.WireOwner{Kind: "project", Name: "ghost"}, manifest.PolicySpec{}),
		rateLimit("rl", manifest.WireOwner{Kind: "project", ID: "ghost"}),
		hostKey("hk", "openai-host", "", manifest.WireOwner{Kind: "project", Name: "ghost"}),
		account("acct", "openai-host", manifest.WireOwner{Kind: "project", Name: "ghost"}),
		pool("pl", "openai-host", manifest.WireOwner{Kind: "project", Name: "ghost"}),
		budget("bd", manifest.WireOwner{Kind: "project", Name: "ghost"}),
	}},
	{name: "rows owned by a present project", add: []manifest.Document{
		policy("pol", inSearch, rateLimits("rl")),
		rateLimit("rl", inSearch),
		budget("bd", inSearch),
	}},
	{name: "service account in a missing project, and in none", add: []manifest.Document{serviceAccount("indexer", "ghost"), serviceAccount("drifter", "")}},
	{name: "keys of a present, a missing and a user principal", add: []manifest.Document{
		serviceAccount("indexer", "ml-search"),
		key("indexer-prod", "serviceaccount", "indexer"),
		key("ghost-key", "serviceaccount", "no-such-account"),
		key("alice-personal", "user", "alice"),
	}},

	// Credentials.
	{name: "host key on a missing host with a missing account", add: []manifest.Document{hostKey("hk", "ghost-host", "ghost-acct", system)}},
	{name: "host key naming no host", add: []manifest.Document{hostKey("hk", "", "", system)}},
	{name: "host key in an account held at another host", add: []manifest.Document{
		host("anthropic-host"), account("acme-anthropic", "anthropic-host", system), hostKey("hk", "openai-host", "acme-anthropic", system),
	}},
	{name: "account at a missing host", add: []manifest.Document{account("acct", "ghost-host", system)}},
	{name: "pool at a missing host with a missing key and one of another host", add: []manifest.Document{
		hostKey("openai-key", "openai-host", "", system),
		pool("mixed", "ghost-host", system, "ghost-key", "openai-key"),
	}},
	{name: "host key reachable through a pool a project policy links", add: []manifest.Document{
		hostKey("openai-key", "openai-host", "", system),
		pool("openai-pool", "openai-host", system, "openai-key"),
		policy("team-pol", inSearch, manifest.PolicySpec{Pools: []string{"openai-pool"}}),
	}},

	// Policies.
	{name: "policy linking missing rows", add: []manifest.Document{policy("user-pol", operator, manifest.PolicySpec{
		Pools:      []string{"ghost-pool"},
		RateLimits: []manifest.PolicyRateLimitDTO{{Ref: "ghost-rl"}, {}},
		Budgets:    []manifest.PolicyBudgetDTO{{Ref: "ghost-budget"}, {}},
	})}},
	{name: "policy model refs", add: []manifest.Document{policy("refs", operator, manifest.PolicySpec{Models: []string{
		"openai", "openai/gpt-x", "*", "gpt-x", "ghost", "openai/ghost-model", "openai/gpt-x@openai-host", "not a ref/@",
	}})}},
	{name: "tier policy scoped to a served model", add: []manifest.Document{rateLimit("tier-rl", atOpenAI), policy("tier", atOpenAI, rateLimits("tier-rl", "openai/gpt-x"))}},
	{name: "tier policy unscoped", add: []manifest.Document{rateLimit("tier-rl", atOpenAI), policy("tier", atOpenAI, rateLimits("tier-rl"))}},
	{name: "tier policy scoped to a model its host does not serve", add: []manifest.Document{rateLimit("tier-rl", atOpenAI), policy("tier", atOpenAI, rateLimits("tier-rl", "openai/gpt-unserved"))}},
	{name: "tier policy scoped to a host", add: []manifest.Document{rateLimit("tier-rl", atOpenAI), policy("tier", atOpenAI, rateLimits("tier-rl", "openai/gpt-x@other"))}},
	{name: "tier policy linking pools", add: []manifest.Document{policy("tier", atOpenAI, manifest.PolicySpec{Pools: []string{"openai-pool"}})}},
	{name: "tier policy of a missing host", add: []manifest.Document{policy("tier", manifest.WireOwner{Kind: "host", Name: "ghost-host"}, manifest.PolicySpec{})}},

	// Role bindings.
	{name: "role binding of a present role at every scope", add: []manifest.Document{
		{Role: &manifest.RoleDTO{Metadata: manifest.WireMeta{Name: "viewer"}}},
		serviceAccount("indexer", "ml-search"),
		roleBinding("global", "viewer", system, manifest.SubjectDTO{Kind: "user", Name: "nobody-checks-users"}, manifest.SubjectDTO{Kind: "serviceaccount", Name: "indexer"}),
		roleBinding("unscoped", "viewer", manifest.WireOwner{}, manifest.SubjectDTO{Kind: "group", Name: "eng"}),
		roleBinding("team", "viewer", inTeam),
		roleBinding("project", "viewer", manifest.WireOwner{Kind: "project", ID: "ml-search"}),
	}},
	{name: "role bindings with missing parts", add: []manifest.Document{
		roleBinding("no-role", "", system),
		roleBinding("ghost-role", "ghost", manifest.WireOwner{Kind: "team", Name: "ghost-team"}, manifest.SubjectDTO{Kind: "serviceaccount", Name: "ghost-sa"}),
		roleBinding("ghost-project", "ghost", manifest.WireOwner{Kind: "project", Name: "ghost-project"}),
		roleBinding("odd-scope", "ghost", manifest.WireOwner{Kind: "host", Name: "openai-host"}),
	}},
}

// policyScopes are the policies a binding may or may not use, by who owns them.
func policyScopes() []manifest.Document {
	return []manifest.Document{
		team("other-team"),
		policy("mine", inSearch, manifest.PolicySpec{}),
		policy("theirs", manifest.WireOwner{Kind: "project", Name: "other"}, manifest.PolicySpec{}),
		policy("team-pol", inTeam, manifest.PolicySpec{}),
		policy("other-team-pol", manifest.WireOwner{Kind: "team", Name: "other-team"}, manifest.PolicySpec{}),
		policy("shared", system, manifest.PolicySpec{}),
		policy("tier", atOpenAI, manifest.PolicySpec{}),
		policy("operator", operator, manifest.PolicySpec{}),
		policy("personal", alice, manifest.PolicySpec{}),
	}
}

// bindingCases mirror the control-plane rule: a binding uses a policy of its
// own scope, an ancestor of it, the system, or a host (a tier preset); a
// personal policy is bound by its owner or a team or project.
var bindingCases = []struct {
	name string
	doc  manifest.Document
}{
	{"own project policy", policyBinding("pb", inSearch, "mine", "acct")},
	{"shared policy", policyBinding("pb", inSearch, "shared", "acct")},
	{"team policy from one of its projects", policyBinding("pb", inSearch, "team-pol", "acct")},
	{"team policy from the team itself", policyBinding("pb", inTeam, "team-pol", "acct")},
	{"host tier policy", policyBinding("pb", inSearch, "tier", "acct")},
	{"another project's policy", policyBinding("pb", inSearch, "theirs", "acct")},
	{"a project's policy from its team", policyBinding("pb", inTeam, "mine", "acct")},
	{"another team's policy", policyBinding("pb", inSearch, "other-team-pol", "acct")},
	{"operator's ownerless shared policy", policyBinding("pb", inSearch, "operator", "acct")},
	{"a project binding a person's policy", policyBinding("pb", inSearch, "personal", "acct")},
	{"the system binding a person's policy", policyBinding("pb", system, "personal", "acct")},
	{"a person binding their own policy", policyBinding("pb", alice, "personal", "acct")},
	{"a person binding another's policy", policyBinding("pb", manifest.WireOwner{Kind: "user", Name: "bob"}, "personal", "acct")},
	{"a binding naming no account", policyBinding("pb", system, "shared")},
	{"a binding naming no policy", policyBinding("pb", system, "", "acct")},
	{"a binding of a missing policy", policyBinding("pb", system, "ghost", "acct")},
	{"a binding in a missing project", policyBinding("pb", manifest.WireOwner{Kind: "project", Name: "ghost"}, "shared", "acct")},
	{"a binding in a missing team", policyBinding("pb", manifest.WireOwner{Kind: "team", ID: "ghost"}, "shared", "acct")},
	{"a binding owned by a host", policyBinding("pb", atOpenAI, "shared", "acct")},
}

func TestValidateGraph(t *testing.T) {
	for _, tc := range graphCases {
		t.Run(tc.name, func(t *testing.T) {
			docs := append(fixture(), tc.add...)
			if tc.edit != nil {
				tc.edit(docs)
			}
			checkReport(t, tc.name, ValidateGraph(docs))
		})
	}
	for _, tc := range bindingCases {
		t.Run(tc.name, func(t *testing.T) {
			checkReport(t, "binding: "+tc.name, ValidateGraph(append(append(fixture(), policyScopes()...), tc.doc)))
		})
	}
}

func checkReport(t *testing.T, name string, issues []Issue) {
	t.Helper()
	got := lines(issues)
	want, ok := wantReports[name]
	if !ok {
		t.Fatalf("no expected report for %q; got:\n%s", name, strings.Join(got, "\n"))
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("report (-want +got):\n%s", diff)
	}
	if HasErrors(issues) != strings.Contains(strings.Join(got, "\n")+"\n", "error ") {
		t.Fatalf("HasErrors = %v for:\n%s", HasErrors(issues), strings.Join(got, "\n"))
	}
}

// wantReports is the whole report of each case, in the order ValidateGraph returns it.
var wantReports = map[string][]string{
	"account at a missing host": {
		"error ref_missing Account/acct.spec.host → Host/ghost-host: host \"ghost-host\" not found",
	},
	"binding adapter that is no upstream shape": {
		"error invariant HostBinding/gpt-x-openai-host.spec.adapter: adapter \"canonical\" is not a valid upstream binding (want one of [openai anthropic gemini openai_responses])",
	},
	"binding naming neither model nor host": {
		"error incomplete HostBinding/gpt-x-openai-host.spec.host: host is required",
		"error incomplete HostBinding/gpt-x-openai-host.spec.model: model is required",
		"warning orphan Model/gpt-x: model has no enabled host bindings; not reachable",
	},
	"binding of a missing model": {
		"error ref_missing HostBinding/gpt-x-openai-host.spec.model → Model/no-such-model: model \"no-such-model\" not found",
		"warning orphan Model/gpt-x: model has no enabled host bindings; not reachable",
	},
	"binding snapshot the model does not declare": {
		"error snapshot_missing HostBinding/gpt-x-openai-host.spec.snapshots[1]: snapshot \"gpt-x-typo\" not declared in model \"gpt-x\" spec.snapshots",
	},
	"binding to a missing host": {
		"error ref_missing HostBinding/gpt-x-openai-host.spec.host → Host/no-such-host: host \"no-such-host\" not found",
	},
	"binding to missing pricing": {
		"error ref_missing HostBinding/gpt-x-openai-host.spec.pricing → Pricing/ghost-price: pricing \"ghost-price\" not found",
	},
	"binding to present pricing": nil,
	"binding: a binding in a missing project": {
		"error ref_missing PolicyBinding/pb.metadata.owner → Project/ghost: project \"ghost\" not found",
	},
	"binding: a binding in a missing team": {
		"error ref_missing PolicyBinding/pb.metadata.owner → Team/ghost: team \"ghost\" not found",
	},
	"binding: a binding naming no account": {
		"error incomplete PolicyBinding/pb.spec.accounts: a policy binding binds at least one account",
	},
	"binding: a binding naming no policy": {
		"error incomplete PolicyBinding/pb.spec.policy: policy is required",
	},
	"binding: a binding of a missing policy": {
		"error ref_missing PolicyBinding/pb.spec.policy → Policy/ghost: policy \"ghost\" not found",
	},
	"binding: a binding owned by a host": {
		"error incomplete PolicyBinding/pb.metadata.owner: a policy binding is owned by a project, a team, a user or the system",
	},
	"binding: a person binding another's policy": {
		"error invariant PolicyBinding/pb.spec.policy → Policy/personal: policy \"personal\" is personal: its owner binds it to their own accounts, or a team or project binds it",
	},
	"binding: a person binding their own policy":   nil,
	"binding: a project binding a person's policy": nil,
	"binding: a project's policy from its team": {
		"error invariant PolicyBinding/pb.spec.policy → Policy/mine: policy \"mine\" belongs to project \"ml-search\", outside this binding's team scope",
	},
	"binding: another project's policy": {
		"error invariant PolicyBinding/pb.spec.policy → Policy/theirs: policy \"theirs\" belongs to project \"other\", outside this binding's project scope",
	},
	"binding: another team's policy": {
		"error invariant PolicyBinding/pb.spec.policy → Policy/other-team-pol: policy \"other-team-pol\" belongs to team \"other-team\", outside this binding's project scope",
	},
	"binding: host tier policy":                     nil,
	"binding: operator's ownerless shared policy":   nil,
	"binding: own project policy":                   nil,
	"binding: shared policy":                        nil,
	"binding: team policy from one of its projects": nil,
	"binding: team policy from the team itself":     nil,
	"binding: the system binding a person's policy": {
		"error invariant PolicyBinding/pb.spec.policy → Policy/personal: policy \"personal\" is personal: its owner binds it to their own accounts, or a team or project binds it",
	},
	"clean fixture": nil,
	"host key in an account held at another host": {
		"error owner_mismatch HostKey/hk.spec.account → Account/acme-anthropic: hostkey on host \"openai-host\" names account \"acme-anthropic\" held at host \"anthropic-host\"",
		"warning orphan HostKey/hk: hostkey not linked by any routable policy or its pools; reachable only through its owner's scope",
	},
	"host key naming no host": {
		"error incomplete HostKey/hk.spec.hostId: hostId is required",
		"warning orphan HostKey/hk: hostkey not linked by any routable policy or its pools; reachable only through its owner's scope",
	},
	"host key on a missing host with a missing account": {
		"error ref_missing HostKey/hk.spec.account → Account/ghost-acct: account \"ghost-acct\" not found",
		"error ref_missing HostKey/hk.spec.hostId → Host/ghost-host: host \"ghost-host\" not found",
		"warning orphan HostKey/hk: hostkey not linked by any routable policy or its pools; reachable only through its owner's scope",
	},
	"host key reachable through a pool a project policy links": nil,
	"keys of a present, a missing and a user principal": {
		"error ref_missing Key/ghost-key.spec.principal → ServiceAccount/no-such-account: service account \"no-such-account\" not found",
	},
	"model of a missing provider, named by id": {
		"error ref_missing Model/gpt-x.metadata.owner → Provider/ghost: owner provider \"ghost\" not found",
		"warning orphan Provider/openai: provider has no models",
	},
	"model owned by the system": {
		"warning orphan Provider/openai: provider has no models",
	},
	"model without snapshots": {
		"error incomplete Model/gpt-x.spec.snapshots: model has no snapshots; nothing is customer-addressable",
	},
	"policy linking missing rows": {
		"error ref_missing Policy/user-pol.spec.budgets[0].ref → Budget/ghost-budget: budget \"ghost-budget\" not found",
		"error incomplete Policy/user-pol.spec.budgets[1].ref: ref is required",
		"error ref_missing Policy/user-pol.spec.pools[0] → Pool/ghost-pool: pool \"ghost-pool\" not found",
		"error ref_missing Policy/user-pol.spec.rateLimits[0].ref → RateLimit/ghost-rl: rateLimit \"ghost-rl\" not found",
		"error incomplete Policy/user-pol.spec.rateLimits[1].ref: ref is required",
	},
	"policy model refs": {
		"error ref_missing Policy/refs.spec.models[3] → Provider/gpt-x: modelref \"gpt-x\" is a bare model name; entries need a provider segment (\"openai/gpt-x\")",
		"error ref_missing Policy/refs.spec.models[4] → Provider/ghost: modelref \"ghost\": provider \"ghost\" not found",
		"error ref_missing Policy/refs.spec.models[5] → Model/ghost-model: modelref \"openai/ghost-model\": model \"ghost-model\" not found",
		"error invariant Policy/refs.spec.models[6]: modelref \"openai/gpt-x@openai-host\" names a host; a policy names a provider or a provider's model, and its pools decide the hosts",
	},
	"pool at a missing host with a missing key and one of another host": {
		"error ref_missing Pool/mixed.spec.credentials[0] → HostKey/ghost-key: hostKey \"ghost-key\" not found",
		"error owner_mismatch Pool/mixed.spec.credentials[1] → HostKey/openai-key: hostkey \"openai-key\" authenticates to host \"openai-host\", not the pool's host \"ghost-host\"",
		"error ref_missing Pool/mixed.spec.host → Host/ghost-host: host \"ghost-host\" not found",
		"warning orphan HostKey/openai-key: hostkey not linked by any routable policy or its pools; reachable only through its owner's scope",
	},
	"pricing for a missing model at a missing host": {
		"error ref_missing Pricing/p1.metadata.owner → Host/ghost-host: owner host \"ghost-host\" not found",
		"error ref_missing Pricing/p1.spec.targetModels[1] → Model/ghost-model: targetModel \"ghost-model\" not found",
	},
	"pricing owned by a host named by id": nil,
	"project naming no team": {
		"error incomplete Project/stray.spec.team: team is required",
	},
	"project of a missing team": {
		"error ref_missing Project/stray.spec.team → Team/ghost-team: team \"ghost-team\" not found",
	},
	"provider without models, model without a binding": {
		"warning orphan Model/gpt-x: model has no enabled host bindings; not reachable",
		"warning orphan Provider/idle: provider has no models",
	},
	"role binding of a present role at every scope": nil,
	"role bindings with missing parts": {
		"error ref_missing RoleBinding/ghost-project.spec.role → Role/ghost: role \"ghost\" not found",
		"error ref_missing RoleBinding/ghost-project.spec.scope → Project/ghost-project: project \"ghost-project\" not found",
		"error ref_missing RoleBinding/ghost-role.spec.role → Role/ghost: role \"ghost\" not found",
		"error ref_missing RoleBinding/ghost-role.spec.scope → Team/ghost-team: team \"ghost-team\" not found",
		"error ref_missing RoleBinding/ghost-role.spec.subjects → ServiceAccount/ghost-sa: service account \"ghost-sa\" not found",
		"error incomplete RoleBinding/no-role.spec.role: role is required",
		"error ref_missing RoleBinding/odd-scope.spec.role → Role/ghost: role \"ghost\" not found",
		"error invariant RoleBinding/odd-scope.spec.scope: scope.kind must be system, team, or project, got \"host\"",
	},
	"rows owned by a missing project": {
		"error ref_missing Account/acct.metadata.owner → Project/ghost: owner project \"ghost\" not found",
		"error ref_missing Budget/bd.metadata.owner → Project/ghost: owner project \"ghost\" not found",
		"error ref_missing HostKey/hk.metadata.owner → Project/ghost: owner project \"ghost\" not found",
		"error ref_missing Policy/pol.metadata.owner → Project/ghost: owner project \"ghost\" not found",
		"error ref_missing Pool/pl.metadata.owner → Project/ghost: owner project \"ghost\" not found",
		"error ref_missing RateLimit/rl.metadata.owner → Project/ghost: owner project \"ghost\" not found",
		"warning orphan HostKey/hk: hostkey not linked by any routable policy or its pools; reachable only through its owner's scope",
		"warning orphan RateLimit/rl: rate limit not referenced by any policy",
	},
	"rows owned by a present project": nil,
	"service account in a missing project, and in none": {
		"error incomplete ServiceAccount/drifter.spec.project: project is required",
		"error ref_missing ServiceAccount/indexer.spec.project → Project/ghost: project \"ghost\" not found",
	},
	"tier policy linking pools": {
		"error invariant Policy/tier.spec.pools: a host-owned tier policy links no pools",
		"error ref_missing Policy/tier.spec.pools[0] → Pool/openai-pool: pool \"openai-pool\" not found",
	},
	"tier policy of a missing host": {
		"error ref_missing Policy/tier.metadata.owner → Host/ghost-host: host \"ghost-host\" not found",
	},
	"tier policy scoped to a host": {
		"error invariant Policy/tier.spec.rateLimits[0].models[0] → Host/openai-host: modelref \"openai/gpt-x@other\" matches no model host \"openai-host\" serves",
		"error invariant Policy/tier.spec.rateLimits[0].models[0]: modelref \"openai/gpt-x@other\" names a host; a policy names a provider or a provider's model, and its pools decide the hosts",
		"error ref_missing Policy/tier.spec.rateLimits[0].models[0] → Host/other: modelref \"openai/gpt-x@other\": host \"other\" not found",
	},
	"tier policy scoped to a model its host does not serve": {
		"error invariant Policy/tier.spec.rateLimits[0].models[0] → Host/openai-host: modelref \"openai/gpt-unserved\" matches no model host \"openai-host\" serves",
		"error ref_missing Policy/tier.spec.rateLimits[0].models[0] → Model/gpt-unserved: modelref \"openai/gpt-unserved\": model \"gpt-unserved\" not found",
	},
	"tier policy scoped to a served model": nil,
	"tier policy unscoped":                 nil,
	"two models with one name": {
		"error duplicate_name Model/gpt-x: more than one Model named \"gpt-x\" in the catalog",
	},
	"two teams with one name, two hosts with one name": {
		"error duplicate_name Host/openai-host: more than one Host named \"openai-host\" in the catalog",
		"error duplicate_name Team/platform: more than one Team named \"platform\" in the catalog",
	},
}

// secondBinding serves the fixture's model at the fixture's host a second time.
func secondBinding() manifest.Document {
	return manifest.Document{HostBinding: &manifest.HostBindingDTO{
		Metadata: manifest.WireMeta{Name: "gpt-x-openai-host-2", Owner: system},
		Spec:     manifest.HostBindingSpec{Model: "gpt-x", Host: "openai-host"},
	}}
}

// One (model, host) pair is served by one binding: a second is reported, naming the other.
func TestValidateGraph_DuplicateBinding(t *testing.T) {
	got := lines(ValidateGraph(append(fixture(), secondBinding())))
	later := `error invariant HostBinding/gpt-x-openai-host-2: duplicate (model="gpt-x", host="openai-host") binding; first declared by "gpt-x-openai-host"`
	earlier := `error invariant HostBinding/gpt-x-openai-host: duplicate (model="gpt-x", host="openai-host") binding; first declared by "gpt-x-openai-host-2"`
	if len(got) != 1 || (got[0] != later && got[0] != earlier) {
		t.Fatalf("report = %q, want the one duplicate", got)
	}
}

// The document declared later is the duplicate, whatever order the graph is walked in.
func TestValidateGraph_DuplicateBindingBlamesTheLaterDocument(t *testing.T) {
	want := []string{`error invariant HostBinding/gpt-x-openai-host-2: duplicate (model="gpt-x", host="openai-host") binding; first declared by "gpt-x-openai-host"`}
	for range 50 {
		if diff := cmp.Diff(want, lines(ValidateGraph(append(fixture(), secondBinding())))); diff != "" {
			t.Fatalf("report (-want +got):\n%s", diff)
		}
	}
}

// Three documents under one name are one mistake, reported once.
func TestValidateGraph_DuplicateNameReportedOncePerName(t *testing.T) {
	got := lines(ValidateGraph(append(fixture(), host("openai-host"), host("openai-host"))))
	want := []string{`error duplicate_name Host/openai-host: more than one Host named "openai-host" in the catalog`}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("report (-want +got):\n%s", diff)
	}
}

func TestFormat(t *testing.T) {
	if got := Format(nil); got != "catalog ok: 0 issues.\n" {
		t.Fatalf("Format(nil) = %q", got)
	}
	issues := []Issue{
		{Severity: SeverityError, Kind: KindRefMissing, Source: Ref{Kind: "Pool", Name: "p", Field: "spec.host"}, Target: Ref{Kind: "Host", Name: "h"}, Message: `host "h" not found`},
		{Severity: SeverityError, Kind: KindIncomplete, Source: Ref{Kind: "Model", Name: "m"}, Message: "no snapshots"},
		{Severity: SeverityWarning, Kind: KindOrphan, Source: Ref{Kind: "Provider", Name: "idle"}, Message: "provider has no models"},
		{Severity: Severity(7), Kind: KindInvariant, Message: "unplaced"},
	}
	want := "error\tref_missing\tPool/p.spec.host → Host/h\thost \"h\" not found\n" +
		"error\tincomplete\tModel/m\tno snapshots\n" +
		"warning\torphan\tProvider/idle\tprovider has no models\n" +
		"?\tinvariant\t\tunplaced\n" +
		"\n2 errors, 1 warnings.\n"
	if got := Format(issues); got != want {
		t.Fatalf("Format =\n%q\nwant\n%q", got, want)
	}
	if !HasErrors(issues) || HasErrors(issues[2:]) || HasErrors(nil) {
		t.Fatal("HasErrors must report exactly the presence of an error-severity issue")
	}
}

func issue(sev Severity, source, msg string) Issue {
	return Issue{Severity: sev, Kind: KindInvariant, Source: Ref{Kind: "Test", Name: source}, Message: msg}
}

// Rules run in order, skipped and check-less ones emit nothing, and the
// findings come back errors first, then by source, then by message.
func TestRunRules(t *testing.T) {
	var ran []string
	rule := func(name string, issues ...Issue) Rule {
		return Rule{Name: name, Check: func(docs []manifest.Document) []Issue {
			ran = append(ran, fmt.Sprintf("%s(%d docs)", name, len(docs)))
			return issues
		}}
	}
	rules := []Rule{
		rule("warns", issue(SeverityWarning, "a", "w")),
		rule("skipped", issue(SeverityError, "a", "never seen")),
		{Name: "no-check"},
		rule("errors", issue(SeverityError, "b", "second"), issue(SeverityError, "b", "first"), issue(SeverityError, "a", "z")),
	}
	got := RunRules(rules, fixture()[:2], map[string]bool{"skipped": true})
	want := []Issue{
		issue(SeverityError, "a", "z"), issue(SeverityError, "b", "first"), issue(SeverityError, "b", "second"),
		issue(SeverityWarning, "a", "w"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("issues (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"warns(2 docs)", "errors(2 docs)"}, ran); diff != "" {
		t.Fatalf("rules run (-want +got):\n%s", diff)
	}
	if got := RunRules(nil, nil, nil); got != nil {
		t.Fatalf("no rules = %v, want nil", got)
	}
}

func TestPromoteAndListRules(t *testing.T) {
	in := []Issue{issue(SeverityWarning, "a", "w"), issue(SeverityError, "b", "e")}
	want := []Issue{issue(SeverityError, "a", "w"), issue(SeverityError, "b", "e")}
	if diff := cmp.Diff(want, Promote(in)); diff != "" {
		t.Fatalf("Promote (-want +got):\n%s", diff)
	}
	if in[0].Severity != SeverityWarning {
		t.Fatal("Promote mutated its input")
	}

	rules := []Rule{{Name: "zebra"}, {Name: "alpha", Description: "first"}, {Name: "mango"}}
	var names []string
	for _, r := range ListRules(rules) {
		names = append(names, r.Name+":"+r.Description)
	}
	if diff := cmp.Diff([]string{"alpha:first", "mango:", "zebra:"}, names); diff != "" || rules[0].Name != "zebra" {
		t.Fatalf("ListRules mutated its input (%q first) or (-want +got):\n%s", rules[0].Name, diff)
	}
}
