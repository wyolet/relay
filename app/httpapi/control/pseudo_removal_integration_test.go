//go:build integration

package control

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// agentRemovalWorld is the policy step world with an agent bob made through POST /pseudo-agents: its account bound alone to its policy, which links its pool at the share host, both labelled for it, and one key. The plans hold all of it.
type agentRemovalWorld struct {
	*policyStepWorld
	agent PseudoAgentCreated
	sink  *auditSink
	em    *audit.Emitter
}

func newAgentRemovalWorld(t *testing.T) *agentRemovalWorld {
	t.Helper()
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	w := &agentRemovalWorld{sink: sink, em: em, policyStepWorld: newPolicyStepWorld(t, func(d *Deps) { d.Audit = em })}
	w.loadPlans(t)
	body := w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}, "key": map[string]any{"displayName": "CI key"}})
	rec := w.postPseudo(t, "bob", "/pseudo-agents", body, false, &w.agent)
	wantStatus(t, rec, http.StatusCreated)
	if w.agent.Policy == nil || len(w.agent.Pools) != 1 || w.agent.Key == nil {
		t.Fatalf("agent = %+v", w.agent)
	}
	w.loadPlans(t)
	return w
}

func (w *agentRemovalWorld) remove(t *testing.T, who string, spec map[string]any, dryRun bool) (*httptest.ResponseRecorder, PseudoAgentRemovalAnswer) {
	t.Helper()
	if _, ok := spec["serviceAccountId"]; !ok {
		spec["serviceAccountId"] = w.agent.ServiceAccount.ID
	}
	var out PseudoAgentRemovalAnswer
	rec := w.postPseudo(t, who, "/pseudo-agent-removals", map[string]any{"spec": spec}, dryRun, &out)
	return rec, out
}

// agentRows are what removing the world's agent deletes, in order.
func (w *agentRemovalWorld) agentRows(t *testing.T) []CreatedRow {
	t.Helper()
	held := w.holdingOf(t, w.agent.Account.ID)
	return []CreatedRow{
		{Kind: "key", ID: w.agent.Key.ID, Name: w.agent.Key.Name},
		{Kind: "policy-binding", ID: held.Meta.ID, Name: held.Meta.Name},
		{Kind: "service-account", ID: w.agent.ServiceAccount.ID, Name: w.agent.ServiceAccount.Name},
		{Kind: "account", ID: w.agent.Account.ID, Name: w.agent.Account.Name},
		{Kind: "policy", ID: w.agent.Policy.ID, Name: w.agent.Policy.Name},
		{Kind: "pool", ID: w.agent.Pools[0].ID, Name: w.agent.Pools[0].Name},
	}
}

// removalAudit closes the world's audit emitter and lists the action and resource id of each row the removal route recorded.
func (w *agentRemovalWorld) removalAudit() []string {
	w.em.Close()
	var out []string
	for _, ev := range w.sink.all() {
		if strings.HasSuffix(ev.Request.Path, "/pseudo-agent-removals") {
			out = append(out, strings.TrimSpace(ev.Action+" "+ev.Resource.ID))
		}
	}
	return out
}

// newIssues lists the plan issues of the current plans that before does not hold, by code and source.
func newIssues(before, after []engine.Issue) []string {
	had := map[string]bool{}
	for _, i := range before {
		had[i.Code+" "+i.Source.ID] = true
	}
	var out []string
	for _, i := range after {
		if !had[i.Code+" "+i.Source.ID] {
			out = append(out, i.Code+" "+string(i.Source.Kind)+" "+i.Source.ID)
		}
	}
	return out
}

func idsOf(rows []CreatedRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// stillStored lists the rows of rows Postgres still holds.
func (w *agentRemovalWorld) stillStored(t *testing.T, rows []CreatedRow) []string {
	t.Helper()
	ctx := t.Context()
	var out []string
	for _, r := range rows {
		var found bool
		var err error
		switch r.Kind {
		case "key":
			var k *key.Key
			k, err = w.stores.Key.Get(ctx, r.ID)
			found = k != nil
		case "policy-binding":
			found = w.stored(t, r.ID, "policy-binding")
		case "service-account":
			sa, e := w.stores.ServiceAccount.Get(ctx, r.ID)
			found, err = sa != nil, e
		case "account":
			a, e := w.stores.Account.Get(ctx, r.ID)
			found, err = a != nil, e
		case "policy":
			p, e := w.stores.Policy.Get(ctx, r.ID)
			found, err = p != nil, e
		case "pool":
			found = w.storedPool(t, r.ID) != nil
		}
		must(t, "read "+r.Kind, err)
		if found {
			out = append(out, r.Kind+"/"+r.Name)
		}
	}
	return out
}

func (w *agentRemovalWorld) stored(t *testing.T, id, kind string) bool {
	t.Helper()
	b, err := w.stores.PolicyBinding.Get(t.Context(), id)
	must(t, kind, err)
	return b != nil
}

// A dry run answers the set and writes nothing; the removal with that set as expect deletes it, each row audited as its own delete, and the key stops authenticating once the plans hear it.
func TestPseudoAgentRemoval_DryRunThenRemove(t *testing.T) {
	w := newAgentRemovalWorld(t)
	want := PseudoAgentRemovalAnswer{Deleted: w.agentRows(t), Kept: []PseudoKept{}, Updated: []CreatedRow{}, Warnings: []PseudoIssue{}}
	before := w.rowCounts(t)
	issues := w.plans.View().AllIssues()
	rec, dry := w.remove(t, "bob", map[string]any{}, true)
	wantStatus(t, rec, http.StatusOK)
	if diff := cmp.Diff(want, dry); diff != "" {
		t.Errorf("dry run (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Fatalf("a dry run wrote rows (-before +after):\n%s", diff)
	}
	hash := apikey.Hash(w.agent.Key.Plaintext)
	if w.holder.Load().Keys.Lookup(hash, time.Now()) == nil {
		t.Fatal("the agent's key does not authenticate before the removal")
	}

	rec, got := w.remove(t, "bob", map[string]any{"expect": idsOf(dry.Deleted)}, false)
	wantStatus(t, rec, http.StatusOK)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("removal (-want +got):\n%s", diff)
	}
	wantActions := []string{
		"keys.delete " + want.Deleted[0].ID, "policy-bindings.delete " + want.Deleted[1].ID, "service-accounts.delete " + want.Deleted[2].ID,
		"policies.delete " + want.Deleted[4].ID, "pools.delete " + want.Deleted[5].ID,
	}
	if diff := cmp.Diff(wantActions, w.removalAudit()); diff != "" {
		t.Errorf("audit, the dry run's none (-want +got):\n%s", diff)
	}
	if left := w.stillStored(t, want.Deleted); len(left) != 0 {
		t.Errorf("still stored: %v", left)
	}
	w.loadPlans(t)
	if kp := w.holder.Load().Keys.Lookup(hash, time.Now()); kp != nil {
		t.Errorf("the deleted key still authenticates as %+v", kp)
	}
	if added := newIssues(issues, w.plans.View().AllIssues()); len(added) != 0 {
		t.Errorf("plan issues the removal added: %v", added)
	}
}

// expect names the set a dry run answered; a key made since changes it, so the removal answers 409 changed with the set as it is now and deletes nothing.
func TestPseudoAgentRemoval_Changed(t *testing.T) {
	w := newAgentRemovalWorld(t)
	_, dry := w.remove(t, "bob", map[string]any{}, true)
	extra := &key.Key{Meta: meta.Metadata{ID: ids.New(), Name: "late-key", Owner: w.owner}, Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: w.agent.ServiceAccount.ID}, AccountID: w.agent.Account.ID, KeyHash: ids.New(), Prefix: "rk_late"}}
	must(t, "late key", w.stores.Key.Upsert(t.Context(), extra))
	before := w.rowCounts(t)
	rec, _ := w.remove(t, "bob", map[string]any{"expect": idsOf(dry.Deleted)}, false)
	wantStatus(t, rec, http.StatusConflict)
	var got removalChanged
	must(t, "decode", json.Unmarshal(rec.Body.Bytes(), &got))
	if got.Err.Code != codeChanged || got.Pointer != "/spec/expect" {
		t.Errorf("refusal = %s", rec.Body)
	}
	wantDeleted := append([]CreatedRow{{Kind: "key", ID: extra.Meta.ID, Name: extra.Meta.Name}}, dry.Deleted...)
	if diff := cmp.Diff(wantDeleted, got.Deleted, cmpopts.SortSlices(func(a, b CreatedRow) bool { return a.ID < b.ID })); diff != "" {
		t.Errorf("deleted now (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]PseudoKept{}, got.Kept); diff != "" {
		t.Errorf("kept now (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a refused removal deleted rows (-before +after):\n%s", diff)
	}
}

// A label is a claim anyone who makes a pool can write: a pool of another team labelled for the agent, which bob may not see, stays and is not named, and keep may not name it; one of another team bob sees stays as not_deletable; one of the agent's scope bob may not see stays as not_visible, without its name. keep leaves the agent's own pool.
func TestPseudoAgentRemoval_LabelsAreClaims(t *testing.T) {
	w := newAgentRemovalWorld(t)
	labelled := func(name string, owner meta.Owner, hidden bool) *pool.Pool {
		p := &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: owner, Labels: map[string]string{pseudo.AgentLabel: w.agent.ServiceAccount.ID}}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{w.teamAcct.Meta.ID}}}
		p.Default()
		must(t, "pool "+name, w.stores.Pool.Upsert(t.Context(), p))
		w.hidden[p.Meta.ID] = hidden
		return p
	}
	one := meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}
	spoof := labelled("one-spoof", one, true)
	seen := labelled("one-seen", one, false)
	hiddenOwn := labelled("two-app-hidden", w.owner, true)
	w.loadPlans(t)
	ownPool := w.agent.Pools[0]

	rec, _ := w.remove(t, "bob", map[string]any{"keep": []string{spoof.Meta.ID}}, true)
	if r := refusalOf(t, rec); rec.Code != http.StatusBadRequest || r.Pointer != "/spec/keep/0" {
		t.Errorf("keep of a hidden row: %d %s", rec.Code, rec.Body)
	}

	rec, got := w.remove(t, "bob", map[string]any{"keep": []string{ownPool.ID}}, false)
	wantStatus(t, rec, http.StatusOK)
	wantKept := []PseudoKept{
		{Kind: "pool", ID: ownPool.ID, Name: ownPool.Name, Reason: pseudo.KeptByRequest},
		{Kind: "pool", ID: seen.Meta.ID, Name: seen.Meta.Name, Reason: pseudo.KeptNotDeletable},
	}
	if diff := cmp.Diff(wantKept, got.Kept, cmpopts.SortSlices(func(a, b PseudoKept) bool { return a.ID < b.ID })); diff != "" || got.KeptHidden != 1 {
		t.Errorf("kept hidden %d, want 1; kept (-want +got):\n%s", got.KeptHidden, diff)
	}
	for _, s := range []string{spoof.Meta.ID, spoof.Meta.Name, hiddenOwn.Meta.ID, hiddenOwn.Meta.Name} {
		if strings.Contains(rec.Body.String(), s) {
			t.Errorf("the answer says %q of a row bob may not see: %s", s, rec.Body)
		}
	}
	for _, p := range []*pool.Pool{spoof, seen, hiddenOwn} {
		if w.storedPool(t, p.Meta.ID) == nil {
			t.Errorf("pool %s was deleted", p.Meta.Name)
		}
	}
	if w.storedPool(t, ownPool.ID) == nil {
		t.Error("the kept pool was deleted")
	}
}

// The agent's pool is linked by a policy of team two bound to team two's own account: the pool stays in use, the policy goes, and team two reaches what it reached before. The after-check runs and finds nothing to refuse.
func TestPseudoAgentRemoval_PoolInUseElsewhere(t *testing.T) {
	w := newAgentRemovalWorld(t)
	ownPool := w.agent.Pools[0]
	w.bind(t, w.policy(t, "two-uses-agent-pool", w.two, policy.Spec{PoolIDs: []string{ownPool.ID}}), w.two, w.teamTwoOwn)
	w.loadPlans(t)
	reach := w.reach(t, w.teamTwoOwn.Meta.ID)
	want := PseudoAgentRemovalAnswer{Deleted: w.agentRows(t)[:5], Kept: []PseudoKept{{Kind: "pool", ID: ownPool.ID, Name: ownPool.Name, Reason: pseudo.KeptInUse}}, Updated: []CreatedRow{}, Warnings: []PseudoIssue{}}

	rec, got := w.remove(t, "bob", map[string]any{}, false)
	wantStatus(t, rec, http.StatusOK)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	w.loadPlans(t)
	if after := w.reach(t, w.teamTwoOwn.Meta.ID); after != reach {
		t.Errorf("team two's reach changed:\nbefore %s\nafter  %s", reach, after)
	}
}

// A share the service account holds is revoked before it goes, and the tier binding holding only that share goes with the revoke.
func TestPseudoAgentRemoval_RevokesHeldShares(t *testing.T) {
	w := newAgentRemovalWorld(t)
	ctx := t.Context()
	sh := account.Share(w.teamTwoAcct, account.Receiver{Kind: account.ReceiverServiceAccount, ID: w.agent.ServiceAccount.ID}, w.owner, "ci-bot")
	sh.Meta.ID = ids.New()
	must(t, "share", w.stores.Account.Upsert(ctx, sh))
	tier := w.policy(t, "share-host-tier", meta.Owner{Kind: meta.OwnerHost, ID: w.host.Meta.ID}, policy.Spec{})
	tb := w.bind(t, tier, w.owner, sh)
	w.loadPlans(t)
	rows := w.agentRows(t)
	want := append(append([]CreatedRow{}, rows[:2]...), CreatedRow{Kind: "account", ID: sh.Meta.ID, Name: sh.Meta.Name}, CreatedRow{Kind: "policy-binding", ID: tb.Meta.ID, Name: tb.Meta.Name})
	want = append(want, rows[2:]...)

	rec, got := w.remove(t, "bob", map[string]any{}, false)
	wantStatus(t, rec, http.StatusOK)
	if diff := cmp.Diff(want, got.Deleted); diff != "" {
		t.Errorf("deleted (-want +got):\n%s", diff)
	}
	if left := w.stillStored(t, want); len(left) != 0 {
		t.Errorf("still stored: %v", left)
	}
}

// A binding a share's revoke leaves empty in a scope the caller may not see goes with the revoke, as DELETE /accounts/by-id/{id} takes it, and is counted in deletedHidden without its id or name.
func TestPseudoAgentRemoval_HiddenRowOfARevoke(t *testing.T) {
	w := newAgentRemovalWorld(t)
	ctx := t.Context()
	sh := account.Share(w.teamTwoAcct, account.Receiver{Kind: account.ReceiverServiceAccount, ID: w.agent.ServiceAccount.ID}, w.owner, "ci-bot")
	sh.Meta.ID = ids.New()
	must(t, "share", w.stores.Account.Upsert(ctx, sh))
	tb := w.bind(t, w.policy(t, "share-host-tier", meta.Owner{Kind: meta.OwnerHost, ID: w.host.Meta.ID}, policy.Spec{}), w.owner, sh)
	w.hidden[tb.Meta.ID] = true
	w.loadPlans(t)
	rows := w.agentRows(t)
	want := append(append(append([]CreatedRow{}, rows[:2]...), CreatedRow{Kind: "account", ID: sh.Meta.ID, Name: sh.Meta.Name}), rows[2:]...)

	for _, dry := range []bool{true, false} {
		rec, got := w.remove(t, "bob", map[string]any{}, dry)
		wantStatus(t, rec, http.StatusOK)
		if diff := cmp.Diff(want, got.Deleted); diff != "" || got.DeletedHidden != 1 {
			t.Errorf("dry run %v: deleted hidden %d, want 1; deleted (-want +got):\n%s", dry, got.DeletedHidden, diff)
		}
		for _, s := range []string{tb.Meta.ID, tb.Meta.Name} {
			if strings.Contains(rec.Body.String(), s) {
				t.Errorf("dry run %v: the answer names %q of a hidden binding", dry, s)
			}
		}
	}
}

// A delete its own call would refuse refuses the request with that refusal at /spec/serviceAccountId; nothing is deleted and one audit row names the route.
func TestPseudoAgentRemoval_Refused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, w *agentRemovalWorld)
		status int
		code   string
	}{
		{"a role binding names the service account", func(t *testing.T, w *agentRemovalWorld) {
			rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "bot-viewer", Owner: w.owner}}
			roles, err := w.stores.Role.List(t.Context())
			must(t, "roles", err)
			for _, r := range roles {
				if r.Meta.Name == "viewer" {
					rb.Spec.RoleID = r.Meta.ID
				}
			}
			yes := true
			rb.Spec.Scope, rb.Spec.Enabled = w.owner, &yes
			rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectServiceAccount, ID: w.agent.ServiceAccount.ID}}
			must(t, "role binding", w.stores.RoleBinding.Upsert(t.Context(), rb))
		}, http.StatusConflict, "resource_in_use"},
		{"the key's delete grant", func(t *testing.T, w *agentRemovalWorld) { *w.denied = "keys.delete" }, http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newAgentRemovalWorld(t)
			tc.setup(t, w)
			defer func() { *w.denied = "" }()
			w.loadPlans(t)
			before := w.rowCounts(t)
			rec, _ := w.remove(t, "bob", map[string]any{}, false)
			r := refusalOf(t, rec)
			if rec.Code != tc.status || r.Err.Code != tc.code || r.Pointer != "/spec/serviceAccountId" {
				t.Errorf("%d %s, want %d %q at /spec/serviceAccountId", rec.Code, rec.Body, tc.status, tc.code)
			}
			if tc.code == "resource_in_use" && len(r.Blockers) == 0 {
				t.Errorf("the refusal carries no blockers: %s", rec.Body)
			}
			if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
				t.Errorf("a refused removal deleted rows (-before +after):\n%s", diff)
			}
			if diff := cmp.Diff([]string{"pseudo-agent-removals.create"}, w.removalAudit()); diff != "" {
				t.Errorf("audit (-want +got):\n%s", diff)
			}
		})
	}
}

// A service account the caller may not see answers 404 at /spec/serviceAccountId, as a missing one does.
func TestPseudoAgentRemoval_NotVisible(t *testing.T) {
	w := newAgentRemovalWorld(t)
	w.hidden[w.agent.ServiceAccount.ID] = true
	rec, _ := w.remove(t, "bob", map[string]any{}, true)
	if r := refusalOf(t, rec); rec.Code != http.StatusNotFound || r.Pointer != "/spec/serviceAccountId" {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}
