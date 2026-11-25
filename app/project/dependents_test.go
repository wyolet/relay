package project

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// dependentRows holds one row of every kind under project "p", one under team "t", and one under project "other" that must never be named. The caller accounts of p and t go with them, a share p received too; a key of another owner running in one of those caller accounts is in the way.
func dependentRows() Rows {
	p, tm, other := meta.Owner{Kind: meta.OwnerProject, ID: "p"}, meta.Owner{Kind: meta.OwnerTeam, ID: "t"}, meta.Owner{Kind: meta.OwnerProject, ID: "other"}
	user := meta.Owner{Kind: meta.OwnerUser, ID: "u"}
	var r Rows
	for _, o := range []meta.Owner{p, tm, other} {
		m := func(kind string) meta.Metadata {
			return meta.Metadata{ID: kind + "-" + o.ID, Name: kind + "-" + o.ID, Owner: o}
		}
		r.Policies = append(r.Policies, &policy.Policy{Meta: m("policy")})
		r.HostKeys = append(r.HostKeys, &hostkey.HostKey{Meta: m("hostkey")})
		r.RateLimits = append(r.RateLimits, &ratelimit.RateLimit{Meta: m("ratelimit")})
		r.PolicyBindings = append(r.PolicyBindings, &policybinding.PolicyBinding{Meta: m("binding")})
		r.Pools = append(r.Pools, &pool.Pool{Meta: m("pool")})
		r.Budgets = append(r.Budgets, &budget.Budget{Meta: m("budget")})
		r.Accounts = append(r.Accounts,
			&account.Account{Meta: m("caller")},
			&account.Account{Meta: m("provider"), Spec: account.Spec{HostID: "h"}},
			&account.Account{Meta: m("share"), Spec: account.Spec{HostID: "h", SourceAccountID: "src"}},
		)
		r.Keys = append(r.Keys, &key.Key{Meta: meta.Metadata{ID: "lane-" + o.ID, Name: "lane-" + o.ID, Owner: user}, Spec: key.Spec{AccountID: "caller-" + o.ID}})
	}
	r.ServiceAccounts = []*serviceaccount.ServiceAccount{
		{Meta: meta.Metadata{ID: "sa-p", Name: "sa-p", Owner: p}, Spec: serviceaccount.Spec{ProjectID: "p"}},
		{Meta: meta.Metadata{ID: "sa-t", Name: "sa-t", Owner: tm}, Spec: serviceaccount.Spec{TeamID: "t"}},
		{Meta: meta.Metadata{ID: "sa-other", Name: "sa-other", Owner: other}, Spec: serviceaccount.Spec{ProjectID: "other"}},
	}
	r.Keys = append(r.Keys, &key.Key{Meta: meta.Metadata{ID: "key-p", Name: "key-p", Owner: p}, Spec: key.Spec{AccountID: "caller-p"}})
	return r
}

func TestDependents(t *testing.T) {
	r := dependentRows()
	projects := []*Project{
		{Meta: meta.Metadata{ID: "p", Name: "p"}, Spec: Spec{TeamID: "t"}},
		{Meta: meta.Metadata{ID: "other", Name: "other"}, Spec: Spec{TeamID: "t-2"}},
	}
	ofProject := "ServiceAccount/sa-p Key/key-p Policy/policy-p HostKey/hostkey-p RateLimit/ratelimit-p Account/provider-p Key/lane-p Pool/pool-p Budget/budget-p PolicyBinding/binding-p"
	for _, tc := range []struct {
		name       string
		run        func(Gone, Visible) ([]string, int)
		gone       Gone
		visible    Visible
		want       string
		wantHidden int
	}{
		{"project", func(g Gone, v Visible) ([]string, int) { return r.Dependents("p", g, v) }, nil, nil, ofProject, 0},
		{"project, rows going in the same operation", func(g Gone, v Visible) ([]string, int) { return r.Dependents("p", g, v) },
			func(kind, id string) bool { return kind == "Key" || id == "pool-p" }, nil,
			"ServiceAccount/sa-p Policy/policy-p HostKey/hostkey-p RateLimit/ratelimit-p Account/provider-p Budget/budget-p PolicyBinding/binding-p", 0},
		{"project, rows the caller cannot see", func(g Gone, v Visible) ([]string, int) { return r.Dependents("p", g, v) },
			nil, func(kind string, m meta.Metadata) bool { return kind == "Key" || m.ID == "pool-p" },
			"Key/key-p Key/lane-p Pool/pool-p", 7},
		{"project, a hidden row that is going is not counted", func(g Gone, v Visible) ([]string, int) { return r.Dependents("p", g, v) },
			func(kind, _ string) bool { return kind != "Budget" }, func(string, meta.Metadata) bool { return false }, "", 1},
		{"a project with nothing under it", func(g Gone, v Visible) ([]string, int) { return r.Dependents("empty", g, v) }, nil, nil, "", 0},
		{"team", func(g Gone, v Visible) ([]string, int) { return r.TeamDependents("t", projects, g, v) }, nil, nil,
			"Project/p ServiceAccount/sa-t Policy/policy-t HostKey/hostkey-t RateLimit/ratelimit-t Account/provider-t Key/lane-t Pool/pool-t Budget/budget-t PolicyBinding/binding-t", 0},
		{"team, its service account going in the same operation", func(g Gone, v Visible) ([]string, int) { return r.TeamDependents("t", projects, g, v) },
			func(kind, id string) bool { return kind == "ServiceAccount" && id == "sa-t" }, nil,
			"Project/p Policy/policy-t HostKey/hostkey-t RateLimit/ratelimit-t Account/provider-t Key/lane-t Pool/pool-t Budget/budget-t PolicyBinding/binding-t", 0},
		{"team, its project going and the rest hidden", func(g Gone, v Visible) ([]string, int) { return r.TeamDependents("t", projects, g, v) },
			func(kind, id string) bool { return kind == "Project" && id == "p" }, func(string, meta.Metadata) bool { return false }, "", 9},
	} {
		named, hidden := tc.run(tc.gone, tc.visible)
		if diff := cmp.Diff(strings.Fields(tc.want), named, cmpEmptyNil); diff != "" || hidden != tc.wantHidden {
			t.Errorf("%s: %d hidden, want %d; named (-want +got):\n%s", tc.name, hidden, tc.wantHidden, diff)
		}
	}
}

var cmpEmptyNil = cmp.Transformer("nilAsEmpty", func(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
})

func TestDependentsError(t *testing.T) {
	twelve := strings.Fields("a b c d e f g h i j k l")
	for _, tc := range []struct {
		name string
		err  DependentsError
		want string
	}{
		{"named rows", DependentsError{Rows: []string{"Key/a", "Pool/b"}}, "still has Key/a, Pool/b: delete or move them first"},
		{"named and hidden rows", DependentsError{Rows: []string{"Key/a"}, Hidden: 2}, "still has Key/a and 2 more: delete or move them first"},
		{"only hidden rows", DependentsError{Hidden: 3}, "still has 3 row(s) under it: delete or move them first"},
		{"ten rows are named in full", DependentsError{Rows: twelve[:10]}, "still has a, b, c, d, e, f, g, h, i, j: delete or move them first"},
		{"more than ten are cut and counted with the hidden", DependentsError{Rows: twelve, Hidden: 1}, "still has a, b, c, d, e, f, g, h, i, j and 3 more: delete or move them first"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
