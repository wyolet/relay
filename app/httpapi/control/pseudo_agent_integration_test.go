//go:build integration

package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// denyAction refuses one action and asks Inner about every other, so a test can refuse one row's own grant; rows whose ids are in hidden are not visible.
type denyAction struct {
	Inner  authz.RBAC
	action *string
	hidden map[string]bool
}

func (d denyAction) Authorize(ctx context.Context, action string, r authz.Resource) error {
	if action == *d.action {
		return authz.ErrForbidden
	}
	return d.Inner.Authorize(ctx, action, r)
}

func (d denyAction) Visible(ctx context.Context, kind, id string, owner meta.Owner) bool {
	return !d.hidden[id] && d.Inner.Visible(ctx, kind, id, owner)
}

func (d denyAction) Unwrap() authz.Authorizer { return d.Inner }

// pseudoAgentWorld adds to the shares world, around project two (team two, administered by bob): a team-two provider account with a resolved key and one with none, bob's personal provider account with a key, a team-two account with a key at a second host, a host that takes no key with a project-two pool and a team-two account, a team-one pool, a policy of project two and one of team one, the team and project accounts, and bob viewing team one. The plans are loaded from Postgres once the rows are in; they hear no later write.
type pseudoAgentWorld struct {
	*sharesWorld
	plans                                           *engine.Engine
	extraAccounts                                   []*account.Account
	denied                                          *string
	hidden                                          map[string]bool
	owner                                           meta.Owner
	teamTwoAcct, emptyAcct, bobAcct, secondHostAcct *account.Account
	keylessAcct                                     *account.Account
	keylessPool, teamOnePool                        *pool.Pool
	projPolicy, teamOnePolicy                       *policy.Policy
}

func newPseudoAgentWorld(t *testing.T, opts ...func(*Deps)) *pseudoAgentWorld {
	t.Helper()
	t.Setenv("PSEUDO_AGENT_TEST_KEY", "sk-pseudo-agent-test-value")
	w := &pseudoAgentWorld{denied: new(string), hidden: map[string]bool{}}
	w.sharesWorld = newSharesWorld(t, append([]func(*Deps){func(d *Deps) {
		wrapped := d.Authz.(audit.Authorizer)
		wrapped.Inner = denyAction{Inner: wrapped.Inner.(authz.RBAC), action: w.denied, hidden: w.hidden}
		d.Authz = wrapped
		d.PlanView = func() *engine.View {
			if w.plans == nil {
				return nil
			}
			return w.plans.View()
		}
		d.PlanCheck = plan.NewCheck(true, time.Minute)
		d.PseudoKinds = []string{"agents"}
	}}, opts...)...)
	ctx := t.Context()
	s := w.stores
	two := meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}
	w.owner = meta.Owner{Kind: meta.OwnerProject, ID: w.projTwo.Meta.ID}
	system := meta.Owner{Kind: meta.OwnerSystem}
	secondHost := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "second-host", Owner: system}, Spec: host.Spec{BaseURL: "https://second.example.com"}}
	keylessHost := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "local-host", Owner: system}, Spec: host.Spec{BaseURL: "http://local.example.com", NoAuth: true}}
	for _, h := range []*host.Host{secondHost, keylessHost} {
		must(t, "host "+h.Meta.Name, s.Host.Upsert(ctx, h))
	}
	providerAcct := func(name string, owner meta.Owner, h *host.Host) *account.Account {
		a := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: owner}, Spec: account.Spec{HostID: h.Meta.ID}}
		must(t, "account "+name, s.Account.Upsert(ctx, a))
		return a
	}
	w.teamTwoAcct = providerAcct("two-provider", two, w.host)
	w.emptyAcct = providerAcct("two-empty", two, w.host)
	w.bobAcct = providerAcct("bob-provider", meta.Owner{Kind: meta.OwnerUser, ID: w.users["bob"]}, w.host)
	w.secondHostAcct = providerAcct("two-second", two, secondHost)
	w.keylessAcct = providerAcct("two-local", two, keylessHost)
	for _, a := range []*account.Account{w.teamTwoAcct, w.bobAcct, w.secondHostAcct} {
		must(t, "key of "+a.Meta.Name, s.HostKey.Upsert(ctx, &hostkey.HostKey{
			Meta: meta.Metadata{ID: ids.New(), Name: a.Meta.Name + "-key", Owner: a.Meta.Owner},
			Spec: hostkey.Spec{HostID: a.Spec.HostID, AccountID: a.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "PSEUDO_AGENT_TEST_KEY"}},
		}))
	}
	w.keylessPool = &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: "two-app-local", Owner: w.owner}, Spec: pool.Spec{HostID: keylessHost.Meta.ID}}
	w.teamOnePool = &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: "one-pool", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{w.teamAcct.Meta.ID}}}
	for _, p := range []*pool.Pool{w.keylessPool, w.teamOnePool} {
		p.Default()
		must(t, "pool "+p.Meta.Name, s.Pool.Upsert(ctx, p))
	}
	w.projPolicy = &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "two-app-policy", Owner: w.owner}}
	w.teamOnePolicy = &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "one-policy", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}}}
	for _, p := range []*policy.Policy{w.projPolicy, w.teamOnePolicy} {
		must(t, "policy "+p.Meta.Name, s.Policy.Upsert(ctx, p))
	}
	for _, a := range []*account.Account{account.ForTeam(w.teamTwo.Meta.ID, w.teamTwo.Meta.Name, ""), account.ForProject(w.projTwo.Meta.ID, w.projTwo.Meta.Name, "")} {
		_, err := s.Account.EnsureManaged(ctx, a)
		must(t, "caller account", err)
	}
	roles, err := s.Role.List(ctx)
	must(t, "roles", err)
	yes := true
	one := meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "bob-viewer-one", Owner: one}}
	rb.Spec.Scope, rb.Spec.Enabled = one, &yes
	for _, r := range roles {
		if r.Meta.Name == "viewer" {
			rb.Spec.RoleID = r.Meta.ID
		}
	}
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: w.users["bob"]}}
	must(t, "bob views team one", s.RoleBinding.Upsert(ctx, rb))
	_, err = s.Account.EnsureManaged(ctx, account.Member(w.users["bob"], "bob", one, "scope", ""))
	must(t, "bob's member account", err)
	must(t, "reload", w.cat.Reload(ctx))
	return w
}

// loadPlans loads the plans from Postgres, with the extra accounts laid in as rows the plans hold and Postgres does not.
func (w *pseudoAgentWorld) loadPlans(t *testing.T) {
	t.Helper()
	cells := cell.NewRegistry(cell.Options{})
	w.plans = engine.New(engine.Options{Cells: cells})
	s := w.stores
	var accounts engine.Source[account.Account] = s.Account
	if len(w.extraAccounts) > 0 {
		accounts = extraRows{Source: s.Account, extra: w.extraAccounts}
	}
	plan.Register(w.plans, cells, plan.Sources{
		Providers: s.Provider, Hosts: s.Host, Models: s.Model, Bindings: s.Binding, Pricings: s.Pricing,
		Policies: s.Policy, PolicyBindings: s.PolicyBinding, RateLimits: s.RateLimit, Budgets: s.Budget, Pools: s.Pool,
		Accounts: accounts, Credentials: s.HostKey, Teams: s.Team, Projects: s.Project, ServiceAccounts: s.ServiceAccount,
		Users: s.Users, Keys: s.Key, Roles: s.Role, RoleBindings: s.RoleBinding, Groups: s.Group, RBAC: true,
	})
	must(t, "load plans", w.plans.Load(t.Context()))
	t.Cleanup(func() { _ = w.plans.Close() })
}

// extraRows is a source with rows beside the store's.
type extraRows struct {
	engine.Source[account.Account]
	extra []*account.Account
}

func (e extraRows) List(ctx context.Context) ([]*account.Account, error) {
	rows, err := e.Source.List(ctx)
	return append(rows, e.extra...), err
}

func (w *pseudoAgentWorld) post(t *testing.T, who string, body map[string]any, dryRun bool) (*httptest.ResponseRecorder, PseudoAgentCreated) {
	t.Helper()
	raw, err := json.Marshal(body)
	must(t, "body", err)
	path := "/pseudo-agents"
	if dryRun {
		path += "?dryRun=true"
	}
	rec := w.send(who, http.MethodPost, path, string(raw))
	var out PseudoAgentCreated
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (w *pseudoAgentWorld) agentBody(spec map[string]any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"displayName": "CI bot", "labels": map[string]string{"sa-type": "agent"}, "owner": map[string]string{"kind": "project", "id": w.owner.ID}},
		"spec":     spec,
	}
}

func refusalOf(t *testing.T, rec *httptest.ResponseRecorder) pseudoRefusal {
	t.Helper()
	var r pseudoRefusal
	must(t, "decode refusal "+rec.Body.String(), json.Unmarshal(rec.Body.Bytes(), &r))
	return r
}

// rowCounts counts the rows of every kind an agent makes.
func (w *pseudoAgentWorld) rowCounts(t *testing.T) map[string]int {
	t.Helper()
	ctx := t.Context()
	s := w.stores
	out := map[string]int{}
	count := func(kind string, n int, err error) {
		must(t, "list "+kind, err)
		out[kind] = n
	}
	sas, err := s.ServiceAccount.List(ctx)
	count("service-account", len(sas), err)
	accs, err := s.Account.List(ctx)
	count("account", len(accs), err)
	pools, err := s.Pool.List(ctx)
	count("pool", len(pools), err)
	pols, err := s.Policy.List(ctx)
	count("policy", len(pols), err)
	pbs, err := s.PolicyBinding.List(ctx)
	count("policy-binding", len(pbs), err)
	keys, err := s.Key.List(ctx)
	count("key", len(keys), err)
	return out
}

func createdKinds(c PseudoAgentCreated) []string {
	var out []string
	for _, r := range c.Created {
		out = append(out, r.Kind+"/"+r.Name)
	}
	return out
}

// An existing policy and a key: the service account in its project, its account bound alone to the policy, a key running there; each audited as its own create.
func TestPseudoAgent_PolicyAndKey(t *testing.T) {
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	w := newPseudoAgentWorld(t, func(d *Deps) { d.Audit = em })
	w.loadPlans(t)
	ctx := t.Context()

	rec, out := w.post(t, "bob", w.agentBody(map[string]any{"policyId": w.projPolicy.Meta.ID, "key": map[string]any{"displayName": "CI bot key"}}), false)
	wantStatus(t, rec, http.StatusCreated)
	em.Close()
	want := []string{"service-account/ci-bot", "account/sa-ci-bot", "policy-binding/two-app-policy-sa-ci-bot", "key/ci-bot-key"}
	if diff := cmp.Diff(want, createdKinds(out)); diff != "" {
		t.Fatalf("created (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(&RowRef{ID: w.projPolicy.Meta.ID, Name: w.projPolicy.Meta.Name}, out.Policy); diff != "" || len(out.Pools) != 0 || len(out.Shares) != 0 {
		t.Errorf("policy (-want +got):\n%s; pools %v shares %v", diff, out.Pools, out.Shares)
	}

	sa, err := w.stores.ServiceAccount.Get(ctx, out.ServiceAccount.ID)
	must(t, "service account", err)
	if sa == nil || sa.Meta.Owner != w.owner || sa.Spec.ProjectID != w.owner.ID || sa.Meta.Labels["sa-type"] != "agent" {
		t.Fatalf("service account = %+v", sa)
	}
	lane, err := w.stores.Account.Get(ctx, out.Account.ID)
	must(t, "lane", err)
	if lane == nil || lane.ManagedKey() != "service:"+sa.Meta.ID || lane.Meta.Owner != w.owner {
		t.Fatalf("lane = %+v", lane)
	}
	held := w.holdingOf(t, lane.Meta.ID)
	if held == nil || held.Spec.PolicyID != w.projPolicy.Meta.ID || !slices.Equal(held.Spec.AccountIDs, []string{lane.Meta.ID}) || held.Meta.Owner != w.owner {
		t.Fatalf("lane held by %+v", held)
	}
	k, err := w.stores.Key.Get(ctx, out.Key.ID)
	must(t, "key", err)
	if k == nil || k.Spec.Principal != (key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}) || k.Spec.AccountID != lane.Meta.ID || out.Key.Plaintext == "" || apikey.Hash(out.Key.Plaintext) != k.Spec.KeyHash {
		t.Fatalf("key = %+v, plaintext %q", k, out.Key.Plaintext)
	}

	var actions []string
	for _, ev := range sink.all() {
		actions = append(actions, ev.Action+" "+ev.Resource.ID)
	}
	wantActions := []string{
		"service-accounts.create " + sa.Meta.ID, "accounts.create " + lane.Meta.ID,
		"policy-bindings.create " + held.Meta.ID, "keys.create " + k.Meta.ID,
	}
	if diff := cmp.Diff(wantActions, actions); diff != "" {
		t.Errorf("audit (-want +got):\n%s", diff)
	}
}

// A write is audited as each row's create; a refused request as one row naming the request and its refusal; a dry run, refused or not, as nothing.
func TestPseudoAgent_Audit(t *testing.T) {
	type entry struct {
		Action, Kind, Status string
		Code                 int
	}
	created := func(kind string) entry {
		return entry{authz.Plural(kind) + ".create", kind, audit.StatusAllowed, http.StatusCreated}
	}
	for _, tc := range []struct {
		name, deny string
		dry, empty bool
		want       []entry
	}{
		{name: "made", want: []entry{created("service-account"), created("account"), created("pool"), created("policy"), created("policy-binding"), created("key")}},
		{name: "a row refused", deny: "pools.create", want: []entry{{"pseudo-agents.create", "pseudo-agent", audit.StatusDenied, http.StatusForbidden}}},
		{name: "refused by the plans", empty: true, want: []entry{{"pseudo-agents.create", "pseudo-agent", audit.StatusError, http.StatusUnprocessableEntity}}},
		{name: "a dry run", dry: true},
		{name: "a refused dry run", deny: "pools.create", dry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &auditSink{}
			em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
			w := newPseudoAgentWorld(t, func(d *Deps) { d.Audit = em })
			w.loadPlans(t)
			*w.denied = tc.deny
			acct := w.teamTwoAcct
			if tc.empty {
				acct = w.emptyAcct
			}
			w.send("bob", http.MethodPost, pseudoPath(tc.dry), mustJSON(t, w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{acct.Meta.ID}}, "key": map[string]any{}})))
			em.Close()
			var got []entry
			for _, ev := range sink.all() {
				if strings.HasSuffix(ev.Request.Path, "/pseudo-agents") {
					got = append(got, entry{ev.Action, ev.Resource.Kind, ev.Outcome.Status, ev.Outcome.Code})
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("audit (-want +got):\n%s", diff)
			}
		})
	}
}

func pseudoPath(dry bool) string {
	if dry {
		return "/pseudo-agents?dryRun=true"
	}
	return "/pseudo-agents"
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	must(t, "body", err)
	return string(raw)
}

func (w *pseudoAgentWorld) holdingOf(t *testing.T, accountID string) *policybinding.PolicyBinding {
	t.Helper()
	b, err := w.stores.PolicyBinding.Holding(t.Context(), accountID)
	must(t, "holding", err)
	return b
}

// Listed accounts: one the project reaches is pooled as it is, a share the project already holds is pooled as it is, and the caller's own account is shared with the project first. Listed again at once, before the plans have seen that share, the share is reused and the request accepted.
func TestPseudoAgent_SharesOnlyWhenNeeded(t *testing.T) {
	w := newPseudoAgentWorld(t)
	rec := w.share("alice", w.alicesAcct, account.ReceiverProject, w.projTwo.Meta.ID)
	wantStatus(t, rec, http.StatusCreated)
	toProject := decodeAccount(t, rec)
	w.loadPlans(t)
	ctx := t.Context()

	listed := []string{w.teamTwoAcct.Meta.ID, toProject.Meta.ID, w.bobAcct.Meta.ID}
	rec, out := w.post(t, "bob", w.agentBody(map[string]any{"access": map[string]any{"accountIds": listed}}), false)
	wantStatus(t, rec, http.StatusCreated)
	if len(out.Shares) != 1 || len(out.Pools) != 1 || out.Policy == nil || out.Key != nil {
		t.Fatalf("answer = %+v, want one share, one pool, a policy and no key", out)
	}
	share, err := w.stores.Account.Get(ctx, out.Shares[0].ID)
	must(t, "share", err)
	if share == nil || share.Spec.SourceAccountID != w.bobAcct.Meta.ID || share.Meta.Owner != w.owner {
		t.Fatalf("share = %+v, want bob's account shared with project two", share)
	}
	p, err := w.stores.Pool.Get(ctx, out.Pools[0].ID)
	must(t, "pool", err)
	if diff := cmp.Diff([]string{w.teamTwoAcct.Meta.ID, toProject.Meta.ID, share.Meta.ID}, p.Spec.AccountIDs); diff != "" || p.Meta.Owner != w.owner || p.Meta.Labels[pseudo.AgentLabel] != out.ServiceAccount.ID {
		t.Fatalf("pool accounts (-want +got):\n%s; pool %+v", diff, p.Meta)
	}
	pol, err := w.stores.Policy.Get(ctx, out.Policy.ID)
	must(t, "policy", err)
	if !slices.Equal(pol.Spec.PoolIDs, []string{p.Meta.ID}) || pol.Meta.Labels[pseudo.AgentLabel] != out.ServiceAccount.ID {
		t.Fatalf("policy = %+v", pol)
	}
	if held := w.holdingOf(t, out.Account.ID); held == nil || held.Spec.PolicyID != pol.Meta.ID {
		t.Fatalf("lane held by %+v", held)
	}

	rec, again := w.post(t, "bob", w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.bobAcct.Meta.ID}}}), false)
	wantStatus(t, rec, http.StatusCreated)
	if len(again.Shares) != 0 {
		t.Fatalf("a second share was made: %+v", again.Shares)
	}
	p2, err := w.stores.Pool.Get(ctx, again.Pools[0].ID)
	must(t, "second pool", err)
	if !slices.Equal(p2.Spec.AccountIDs, []string{share.Meta.ID}) {
		t.Fatalf("second pool lists %v, want the share made first", p2.Spec.AccountIDs)
	}
}

// The policy links the named pools first, in the order given, then the pools made, one per host in the order each host first appears in accountIds, each listing its accounts in the order given.
func TestPseudoAgent_PoolOrder(t *testing.T) {
	w := newPseudoAgentWorld(t)
	w.loadPlans(t)
	ctx := t.Context()
	listed := []string{w.teamTwoAcct.Meta.ID, w.secondHostAcct.Meta.ID, w.bobAcct.Meta.ID}
	rec, out := w.post(t, "bob", w.agentBody(map[string]any{"access": map[string]any{"poolIds": []string{w.keylessPool.Meta.ID}, "accountIds": listed}}), false)
	wantStatus(t, rec, http.StatusCreated)
	if len(out.Pools) != 2 || len(out.Shares) != 1 {
		t.Fatalf("answer = %+v, want two pools and bob's share", out)
	}
	pol, err := w.stores.Policy.Get(ctx, out.Policy.ID)
	must(t, "policy", err)
	if diff := cmp.Diff([]string{w.keylessPool.Meta.ID, out.Pools[0].ID, out.Pools[1].ID}, pol.Spec.PoolIDs); diff != "" {
		t.Errorf("policy pools (-want +got):\n%s", diff)
	}
	members := func(id string) []string {
		p, err := w.stores.Pool.Get(ctx, id)
		must(t, "pool", err)
		return p.Spec.AccountIDs
	}
	if diff := cmp.Diff([]string{w.teamTwoAcct.Meta.ID, out.Shares[0].ID}, members(out.Pools[0].ID)); diff != "" {
		t.Errorf("first host's pool (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{w.secondHostAcct.Meta.ID}, members(out.Pools[1].ID)); diff != "" {
		t.Errorf("second host's pool (-want +got):\n%s", diff)
	}
}

// A dry run answers what would be made, ids included and the plaintext left out, and writes nothing.
func TestPseudoAgent_DryRunWritesNothing(t *testing.T) {
	w := newPseudoAgentWorld(t)
	w.loadPlans(t)
	before := w.rowCounts(t)
	body := w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID, w.bobAcct.Meta.ID}}, "key": map[string]any{"displayName": "CI bot key"}})

	rec, dry := w.post(t, "bob", body, true)
	wantStatus(t, rec, http.StatusOK)
	if dry.Key == nil || dry.Key.ID == "" || dry.Key.Plaintext != "" || dry.ServiceAccount.ID == "" || len(dry.Shares) != 1 {
		t.Fatalf("dry run = %+v", dry)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Fatalf("a dry run wrote rows (-before +after):\n%s", diff)
	}
	if sa, err := w.stores.ServiceAccount.Get(t.Context(), dry.ServiceAccount.ID); err != nil || sa != nil {
		t.Fatalf("dry run's service account stored: %+v %v", sa, err)
	}

	rec, made := w.post(t, "bob", body, false)
	wantStatus(t, rec, http.StatusCreated)
	if diff := cmp.Diff(createdKinds(dry), createdKinds(made)); diff != "" {
		t.Errorf("the write made other rows than the dry run answered (-dry +made):\n%s", diff)
	}
}

// Each row passes its own create call's checks: a refusal names the row by pointer, answers as that call would, and leaves nothing written, the last row's included.
func TestPseudoAgent_RowRefusals(t *testing.T) {
	w := newPseudoAgentWorld(t)
	rec := w.share("alice", w.alicesAcct, account.ReceiverUser, w.users["bob"])
	wantStatus(t, rec, http.StatusCreated)
	toBob := decodeAccount(t, rec)
	w.loadPlans(t)
	before := w.rowCounts(t)
	accounts := func(ids ...string) map[string]any {
		return map[string]any{"access": map[string]any{"accountIds": ids}, "key": map[string]any{}}
	}
	reached := accounts(w.teamTwoAcct.Meta.ID)

	for _, tc := range []struct {
		name, who, deny string
		spec            map[string]any
		status          int
		pointer         string
	}{
		{"a viewer may not create the service account", "alice", "", reached, http.StatusForbidden, "/metadata"},
		{"the service account's grant", "bob", "service-accounts.create", reached, http.StatusForbidden, "/metadata"},
		{"an account the caller may not see", "bob", "", accounts(w.alicesAcct.Meta.ID), http.StatusNotFound, "/spec/access/accountIds/0"},
		{"an account the caller sees but may not share", "bob", "", accounts(w.teamTwoAcct.Meta.ID, w.teamAcct.Meta.ID), http.StatusForbidden, "/spec/access/accountIds/1"},
		{"a share made with another receiver", "bob", "", accounts(toBob.Meta.ID), http.StatusBadRequest, "/spec/access/accountIds"},
		{"an account by slug", "bob", "", accounts(w.teamTwoAcct.Meta.Name), http.StatusBadRequest, "/spec/access/accountIds/0"},
		{"a pool by slug", "bob", "", map[string]any{"access": map[string]any{"poolIds": []string{w.keylessPool.Meta.Name}}}, http.StatusBadRequest, "/spec/access/poolIds/0"},
		{"a policy by slug", "bob", "", map[string]any{"policyId": w.projPolicy.Meta.Name}, http.StatusBadRequest, "/spec/policyId"},
		{"an account at a host that takes no key", "bob", "", accounts(w.teamTwoAcct.Meta.ID, w.keylessAcct.Meta.ID), http.StatusBadRequest, "/spec/access/accountIds/1"},
		{"a pool the project may not link", "bob", "", map[string]any{"access": map[string]any{"poolIds": []string{w.keylessPool.Meta.ID, w.teamOnePool.Meta.ID}}}, http.StatusBadRequest, "/spec/access/poolIds/1"},
		{"the pool's grant", "bob", "pools.create", reached, http.StatusForbidden, "/spec/access/accountIds"},
		{"the policy's grant", "bob", "policies.create", reached, http.StatusForbidden, "/spec/access"},
		{"the binding's grant", "bob", "policy-bindings.create", reached, http.StatusForbidden, "/spec/access"},
		{"a policy the project may not bind", "bob", "", map[string]any{"policyId": w.teamOnePolicy.Meta.ID}, http.StatusBadRequest, "/spec/policyId"},
		{"the key's grant, the last row", "bob", "keys.create", reached, http.StatusForbidden, "/spec/key"},
		{"both policyId and access", "bob", "", map[string]any{"policyId": w.projPolicy.Meta.ID, "access": map[string]any{"everyCredential": true}}, http.StatusBadRequest, "/spec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*w.denied = tc.deny
			defer func() { *w.denied = "" }()
			rec, _ := w.post(t, tc.who, w.agentBody(tc.spec), false)
			if rec.Code != tc.status {
				t.Fatalf("status %d %s, want %d", rec.Code, rec.Body, tc.status)
			}
			if got := refusalOf(t, rec).Pointer; got != tc.pointer {
				t.Errorf("pointer = %q, want %q", got, tc.pointer)
			}
			if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
				t.Errorf("a refused request wrote rows (-before +after):\n%s", diff)
			}
		})
	}
}

// Rows that each pass their own checks but together leave the policy no usable key are refused with the issue on the policy, pointed at; nothing is written.
func TestPseudoAgent_PlanIssue(t *testing.T) {
	w := newPseudoAgentWorld(t)
	w.loadPlans(t)
	before := w.rowCounts(t)
	rec, _ := w.post(t, "bob", w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.emptyAcct.Meta.ID}}}), false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d %s, want 422", rec.Code, rec.Body)
	}
	r := refusalOf(t, rec)
	if r.Err.Code != codePlanIssue {
		t.Fatalf("code = %q, want plan_issue", r.Err.Code)
	}
	var onPolicy []PseudoIssue
	for _, i := range r.Issues {
		if i.Code == plan.IssuePoolsServeNothing {
			i.ID, i.Message = "", ""
			onPolicy = append(onPolicy, i)
		}
	}
	want := []PseudoIssue{{Code: plan.IssuePoolsServeNothing, Kind: string(plan.KindPolicy), Name: "ci-bot", Field: "spec.poolIds", Pointer: "/spec/access"}}
	if diff := cmp.Diff(want, onPolicy); diff != "" {
		t.Errorf("issues (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a refused request wrote rows (-before +after):\n%s", diff)
	}
}

// Listing an account the caller may not share answers the same whether or not the owner already holds a share of it the caller may not see.
func TestPseudoAgent_HiddenShareIsNotUsed(t *testing.T) {
	w := newPseudoAgentWorld(t)
	rec := w.share("dave", w.teamAcct, account.ReceiverProject, w.projTwo.Meta.ID)
	wantStatus(t, rec, http.StatusCreated)
	w.hidden[decodeAccount(t, rec).Meta.ID] = true
	w.loadPlans(t)
	rec, _ = w.post(t, "bob", w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.teamAcct.Meta.ID}}}), false)
	wantStatus(t, rec, http.StatusForbidden)
	if r := refusalOf(t, rec); r.Pointer != "/spec/access/accountIds/0" {
		t.Errorf("pointer = %q, want the account's", r.Pointer)
	}
}

// A body without an owner is refused for that, with its pointer; there is no default owner.
func TestPseudoAgent_OwnerRequired(t *testing.T) {
	w := newPseudoAgentWorld(t)
	w.loadPlans(t)
	body := w.agentBody(map[string]any{"policyId": w.projPolicy.Meta.ID})
	delete(body["metadata"].(map[string]any), "owner")
	rec, _ := w.post(t, "bob", body, false)
	wantStatus(t, rec, http.StatusBadRequest)
	if r := refusalOf(t, rec); r.Pointer != "/metadata/owner" || r.Err.Message != "metadata.owner is required: the kind and id of the scope that owns the service account" {
		t.Errorf("refusal = %+v", r)
	}
}

// A disabled policy files no error, but every request of a lane bound to it is refused: the request is refused, on the binding.
func TestPseudoAgent_DisabledPolicy(t *testing.T) {
	w := newPseudoAgentWorld(t)
	off := false
	disabled := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "two-app-off", Owner: w.owner}, Spec: policy.Spec{Enabled: &off}}
	must(t, "disabled policy", w.stores.Policy.Upsert(t.Context(), disabled))
	w.loadPlans(t)
	before := w.rowCounts(t)
	rec, _ := w.post(t, "bob", w.agentBody(map[string]any{"policyId": disabled.Meta.ID, "key": map[string]any{}}), false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d %s, want 422", rec.Code, rec.Body)
	}
	var codes []string
	for _, i := range refusalOf(t, rec).Issues {
		codes = append(codes, i.Code+" "+i.Kind+" "+i.Pointer)
	}
	if diff := cmp.Diff([]string{"policy_disabled policybinding /spec/policyId"}, codes); diff != "" {
		t.Errorf("issues (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a refused request wrote rows (-before +after):\n%s", diff)
	}
}

// A host key written after the plans loaded counts: its account, keyless in the plans, serves the pool made from it.
func TestPseudoAgent_KeyWrittenAfterThePlans(t *testing.T) {
	w := newPseudoAgentWorld(t)
	w.loadPlans(t)
	must(t, "late key", w.stores.HostKey.Upsert(t.Context(), &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "two-empty-key", Owner: w.emptyAcct.Meta.Owner},
		Spec: hostkey.Spec{HostID: w.host.Meta.ID, AccountID: w.emptyAcct.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "PSEUDO_AGENT_TEST_KEY"}},
	}))
	rec, _ := w.post(t, "bob", w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.emptyAcct.Meta.ID}}}), false)
	wantStatus(t, rec, http.StatusCreated)
}

// A row read fresh that is really broken still refuses the rows it breaks, and what it breaks among existing rows is not laid on the request: a host disabled after the plans loaded.
func TestPseudoAgent_FreshRowBroken(t *testing.T) {
	w := newPseudoAgentWorld(t)
	w.loadPlans(t)
	off := false
	disabled := *w.host
	disabled.Spec.Enabled = &off
	must(t, "disable host", w.stores.Host.Upsert(t.Context(), &disabled))
	before := w.rowCounts(t)
	rec, _ := w.post(t, "bob", w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}}), false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d %s, want 422", rec.Code, rec.Body)
	}
	issues := refusalOf(t, rec).Issues
	if len(issues) == 0 {
		t.Fatal("no issues listed")
	}
	for _, i := range issues {
		if i.Pointer == "" {
			t.Errorf("an issue on a row the request did not make: %+v", i)
		}
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a refused request wrote rows (-before +after):\n%s", diff)
	}
}

// Rows that would break a row the plans serve are refused through the issue on that row, which has no pointer.
func TestPseudoAgent_BreaksAnExistingRow(t *testing.T) {
	w := newPseudoAgentWorld(t)
	w.extraAccounts = []*account.Account{{Meta: meta.Metadata{ID: ids.New(), Name: "sa-ci-bot", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}}, Spec: account.Spec{HostID: w.host.Meta.ID}}}
	w.loadPlans(t)
	before := w.rowCounts(t)
	rec, _ := w.post(t, "bob", w.agentBody(map[string]any{"policyId": w.projPolicy.Meta.ID}), false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d %s, want 422", rec.Code, rec.Body)
	}
	var onExisting []PseudoIssue
	for _, i := range refusalOf(t, rec).Issues {
		if i.ID == w.extraAccounts[0].Meta.ID {
			i.Message = ""
			onExisting = append(onExisting, i)
		}
	}
	want := []PseudoIssue{{Code: "duplicate_name", Kind: string(plan.KindAccount), ID: w.extraAccounts[0].Meta.ID, Name: "sa-ci-bot", Field: "metadata.name"}}
	if diff := cmp.Diff(want, onExisting); diff != "" {
		t.Errorf("issues on the existing account (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a refused request wrote rows (-before +after):\n%s", diff)
	}
}

// Concurrent identical requests each land whole or answer 409; every service account made has its lane, binding and key.
func TestPseudoAgent_Concurrent(t *testing.T) {
	w := newPseudoAgentWorld(t)
	w.loadPlans(t)
	before := w.rowCounts(t)
	const calls = 6
	codes := make([]int, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Go(func() {
			rec, _ := w.post(t, "bob", w.agentBody(map[string]any{"policyId": w.projPolicy.Meta.ID, "key": map[string]any{}}), false)
			codes[i] = rec.Code
		})
	}
	wg.Wait()
	t.Logf("codes: %v", codes)
	made := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			made++
		case http.StatusConflict:
		default:
			t.Fatalf("codes = %v, want only 201 and 409", codes)
		}
	}
	if made == 0 {
		t.Fatalf("codes = %v, want at least one through", codes)
	}
	after := w.rowCounts(t)
	for _, kind := range []string{"service-account", "account", "policy-binding", "key"} {
		if after[kind]-before[kind] != made {
			t.Errorf("%d new %s rows for %d requests through", after[kind]-before[kind], kind, made)
		}
	}
}
