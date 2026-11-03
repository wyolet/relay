//go:build integration

package control

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/ids"
)

// policyWorld adds to the shares world two policies of team one, one of team two, and a team-one binding holding team one's provider account and dave's member account there.
type policyWorld struct {
	*sharesWorld
	polA, polB, farPol *policy.Policy
	daveAcct           string
	shared             *policybinding.PolicyBinding
}

func newPolicyWorld(t *testing.T, opts ...func(*Deps)) *policyWorld {
	t.Helper()
	ctx := t.Context()
	w := &policyWorld{sharesWorld: newSharesWorld(t, opts...)}
	teamOne := meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}
	mkPolicy := func(name string, owner meta.Owner) *policy.Policy {
		p := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: owner}}
		must(t, "policy "+name, w.stores.Policy.Upsert(ctx, p))
		return p
	}
	w.polA, w.polB = mkPolicy("pol-a", teamOne), mkPolicy("pol-b", teamOne)
	w.farPol = mkPolicy("pol-far", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID})
	var err error
	w.daveAcct, err = w.stores.Account.EnsureManaged(ctx, account.Member(w.users["dave"], "dave", teamOne, "scope", ""))
	must(t, "dave's member account", err)
	w.shared = &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: ids.New(), Name: "team-one-shared", Owner: teamOne},
		Spec: policybinding.Spec{PolicyID: w.polA.Meta.ID, AccountIDs: []string{w.teamAcct.Meta.ID, w.daveAcct}},
	}
	must(t, "shared binding", w.stores.PolicyBinding.Upsert(ctx, w.shared))
	must(t, "reload", w.cat.Reload(ctx))
	return w
}

func (w *policyWorld) setPolicy(who, accountID string, policyID *string) (*httptest.ResponseRecorder, AccountPolicy) {
	body := `{"policyId":null}`
	if policyID != nil {
		body = `{"policyId":"` + *policyID + `"}`
	}
	rec := w.send(who, http.MethodPut, "/accounts/"+accountID+"/policy", body)
	var out AccountPolicy
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (w *policyWorld) holding(t *testing.T, accountID string) *policybinding.PolicyBinding {
	t.Helper()
	b, err := w.stores.PolicyBinding.Holding(t.Context(), accountID)
	must(t, "holding", err)
	return b
}

func (w *policyWorld) binding(t *testing.T, id string) *policybinding.PolicyBinding {
	t.Helper()
	b, err := w.stores.PolicyBinding.Get(t.Context(), id)
	must(t, "get binding", err)
	return b
}

// Moving an account out of a binding that holds others leaves them there and binds the account alone; moving the last one out deletes the old binding; null unbinds; the same policy again changes nothing.
func TestAccountPolicy_MoveUnbindAndNoop(t *testing.T) {
	w := newPolicyWorld(t)
	teamOne := meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}
	acct := w.teamAcct.Meta.ID

	rec, out := w.setPolicy("dave", acct, &w.polB.Meta.ID)
	wantStatus(t, rec, http.StatusOK)
	nb := out.Binding
	if nb == nil || nb.Spec.PolicyID != w.polB.Meta.ID || !slices.Equal(nb.Spec.AccountIDs, []string{acct}) || nb.Meta.Owner != teamOne || !nb.IsEnabled() {
		t.Fatalf("new binding = %+v", nb)
	}
	if nb.Meta.Name != "pol-b-"+w.teamAcct.Meta.Name {
		t.Errorf("new binding name = %q", nb.Meta.Name)
	}
	if out.PreviousBindingID != w.shared.Meta.ID || out.PreviousBindingDeleted {
		t.Fatalf("previous = %q deleted %v, want the shared binding kept", out.PreviousBindingID, out.PreviousBindingDeleted)
	}
	if left := w.binding(t, w.shared.Meta.ID); left == nil || !slices.Equal(left.Spec.AccountIDs, []string{w.daveAcct}) {
		t.Fatalf("shared binding after the move = %+v, want only dave's account", left)
	}
	if h := w.holding(t, acct); h == nil || h.Meta.ID != nb.Meta.ID {
		t.Fatalf("account held by %+v, want the new binding", h)
	}

	// The last account out of the shared binding deletes it.
	rec, out = w.setPolicy("dave", w.daveAcct, &w.polB.Meta.ID)
	wantStatus(t, rec, http.StatusOK)
	if out.PreviousBindingID != w.shared.Meta.ID || !out.PreviousBindingDeleted {
		t.Fatalf("previous = %q deleted %v, want the shared binding deleted", out.PreviousBindingID, out.PreviousBindingDeleted)
	}
	if w.binding(t, w.shared.Meta.ID) != nil {
		t.Fatal("the emptied binding is still there")
	}
	// It never joins the other binding of the same policy.
	if out.Binding == nil || out.Binding.Meta.ID == nb.Meta.ID || !slices.Equal(out.Binding.Spec.AccountIDs, []string{w.daveAcct}) {
		t.Fatalf("dave's binding = %+v, want one of its own", out.Binding)
	}

	// The same policy again changes nothing and answers the binding.
	rec, out = w.setPolicy("dave", acct, &w.polB.Meta.ID)
	wantStatus(t, rec, http.StatusOK)
	if out.Binding == nil || out.Binding.Meta.ID != nb.Meta.ID || out.PreviousBindingID != "" {
		t.Fatalf("same policy = %+v, want the binding unchanged", out)
	}

	rec, out = w.setPolicy("dave", acct, nil)
	wantStatus(t, rec, http.StatusOK)
	if out.Binding != nil || out.PreviousBindingID != nb.Meta.ID || !out.PreviousBindingDeleted {
		t.Fatalf("unbind = %+v, want no binding and the old one deleted", out)
	}
	if h := w.holding(t, acct); h != nil {
		t.Fatalf("unbound account still held by %s", h.Meta.Name)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || string(raw["binding"]) != "null" {
		t.Errorf("unbind body = %s, want binding null", rec.Body)
	}

	// Unbinding an unbound account changes nothing.
	rec, out = w.setPolicy("dave", acct, nil)
	wantStatus(t, rec, http.StatusOK)
	if out.Binding != nil || out.PreviousBindingID != "" {
		t.Fatalf("unbind again = %+v, want nothing", out)
	}
}

// A caller who may not update the holding binding, or a policy the account's scope may not use, changes nothing: the whole call rolls back with the answer the separate write would give.
func TestAccountPolicy_Refusals(t *testing.T) {
	w := newPolicyWorld(t)
	acct := w.teamAcct.Meta.ID
	unchanged := func(t *testing.T) {
		t.Helper()
		b := w.binding(t, w.shared.Meta.ID)
		if b == nil || !slices.Equal(b.Spec.AccountIDs, []string{acct, w.daveAcct}) || b.Spec.PolicyID != w.polA.Meta.ID {
			t.Fatalf("shared binding = %+v, want it untouched", b)
		}
		all, err := w.stores.PolicyBinding.List(t.Context())
		must(t, "list", err)
		if len(all) != 1 {
			t.Fatalf("%d bindings, want only the shared one", len(all))
		}
	}

	// erin views team one: she sees the binding but may not update it.
	if rec, _ := w.setPolicy("erin", acct, &w.polB.Meta.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer's change: %d %s, want 403", rec.Code, rec.Body)
	}
	unchanged(t)

	// A team-one binding cannot use team two's policy, which dave sees as a viewer there.
	if rec, _ := w.setPolicy("dave", acct, &w.farPol.Meta.ID); rec.Code != http.StatusBadRequest {
		t.Fatalf("another team's policy: %d %s, want 400", rec.Code, rec.Body)
	}
	unchanged(t)

	missing := ids.New()
	if rec, _ := w.setPolicy("dave", acct, &missing); rec.Code != http.StatusBadRequest {
		t.Fatalf("a policy that does not exist: %d %s, want 400", rec.Code, rec.Body)
	}
	unchanged(t)

	if rec, _ := w.setPolicy("carol", acct, &w.polB.Meta.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger's change: %d %s, want 404", rec.Code, rec.Body)
	}
	unchanged(t)
}

// Each write is audited as its own policy-bindings call would be: a move out of a binding holding others as its update plus the create, a move out of the last account's binding as its delete plus the create.
func TestAccountPolicy_Audit(t *testing.T) {
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	w := newPolicyWorld(t, func(d *Deps) { d.Audit = em })

	rec, fromShared := w.setPolicy("dave", w.teamAcct.Meta.ID, &w.polB.Meta.ID)
	wantStatus(t, rec, http.StatusOK)
	rec, fromOwn := w.setPolicy("dave", w.daveAcct, &w.polB.Meta.ID)
	wantStatus(t, rec, http.StatusOK)
	em.Close()

	type entry struct{ action, id string }
	var got []entry
	for _, ev := range sink.all() {
		if ev.Actor.ID != w.users["dave"] || ev.Outcome.Status != audit.StatusAllowed || ev.Outcome.Code != http.StatusOK || ev.Resource.Kind != "policy-binding" {
			t.Errorf("event %s on %s: actor %+v outcome %+v kind %q", ev.Action, ev.Resource.ID, ev.Actor, ev.Outcome, ev.Resource.Kind)
		}
		var fields []string
		if ev.Change != nil {
			fields = ev.Change.Fields
		}
		want := []string{audit.AnyField}
		if ev.Action == "policy-bindings.update" {
			want = []string{"spec.accountIds"}
		}
		if !slices.Equal(fields, want) {
			t.Errorf("%s on %s changed %v, want %v", ev.Action, ev.Resource.ID, fields, want)
		}
		got = append(got, entry{ev.Action, ev.Resource.ID})
	}
	want := []entry{
		{"policy-bindings.update", w.shared.Meta.ID},
		{"policy-bindings.create", fromShared.Binding.Meta.ID},
		{"policy-bindings.delete", w.shared.Meta.ID},
		{"policy-bindings.create", fromOwn.Binding.Meta.ID},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit entries = %v, want %v", got, want)
	}
}

// Concurrent calls on one account never leave it in two bindings or none: each lands whole or answers 409.
func TestAccountPolicy_Concurrent(t *testing.T) {
	w := newPolicyWorld(t)
	acct := w.teamAcct.Meta.ID
	const calls = 8
	codes := make([]int, calls)
	var wg sync.WaitGroup
	for i := range calls {
		pol := &w.polA.Meta.ID
		if i%2 == 0 {
			pol = &w.polB.Meta.ID
		}
		wg.Go(func() {
			rec, _ := w.setPolicy("dave", acct, pol)
			codes[i] = rec.Code
		})
	}
	wg.Wait()
	t.Logf("codes: %v", codes)
	ok := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
		default:
			t.Fatalf("codes = %v, want only 200 and 409", codes)
		}
	}
	if ok == 0 {
		t.Fatalf("codes = %v, want at least one call through", codes)
	}
	if h := w.holding(t, acct); h == nil || !slices.Equal(h.Spec.AccountIDs, []string{acct}) {
		t.Fatalf("account held by %+v, want a binding of its own", h)
	}
	all, err := w.stores.PolicyBinding.List(t.Context())
	must(t, "list", err)
	held := 0
	for _, b := range all {
		if len(b.Spec.AccountIDs) == 0 {
			t.Fatalf("binding %s holds no account", b.Meta.Name)
		}
		if slices.Contains(b.Spec.AccountIDs, acct) {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("account held by %d bindings, want 1", held)
	}
}
