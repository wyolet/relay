//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

func (w *policyWorld) setAccounts(who, policyRef string, accountIDs ...string) (*httptest.ResponseRecorder, PolicyAccounts) {
	body, _ := json.Marshal(map[string][]string{"accountIds": append([]string{}, accountIDs...)})
	rec := w.send(who, http.MethodPut, "/policies/"+policyRef+"/accounts", string(body))
	var out PolicyAccounts
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// The listed accounts end bound to the policy, unlisted ones bound to it are unbound, and the same list again changes nothing.
func TestPolicyAccounts_SetExactSet(t *testing.T) {
	w := newPolicyWorld(t)
	team, dave := w.teamAcct.Meta.ID, w.daveAcct

	// Both accounts leave polA's shared binding; the second one out deletes it.
	rec, out := w.setAccounts("dave", w.polB.Meta.Name, team, dave, team)
	wantStatus(t, rec, http.StatusOK)
	if len(out.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want the two listed, deduplicated", out.Accounts)
	}
	for i, a := range out.Accounts {
		if a.Binding == nil || a.Binding.Spec.PolicyID != w.polB.Meta.ID || a.PreviousBindingID != w.shared.Meta.ID || a.Removed {
			t.Fatalf("account %d = %+v", i, a)
		}
	}
	if out.Accounts[0].PreviousBindingDeleted || !out.Accounts[1].PreviousBindingDeleted {
		t.Fatalf("deleted flags = %v %v, want the second move to delete the shared binding", out.Accounts[0].PreviousBindingDeleted, out.Accounts[1].PreviousBindingDeleted)
	}
	if w.binding(t, w.shared.Meta.ID) != nil {
		t.Fatal("the emptied binding is still there")
	}

	// Leaving dave out unbinds his account and keeps the team's.
	rec, out = w.setAccounts("dave", w.polB.Meta.ID, team)
	wantStatus(t, rec, http.StatusOK)
	if len(out.Accounts) != 2 || out.Accounts[0].PreviousBindingID != "" || !out.Accounts[1].Removed || out.Accounts[1].AccountID != dave || out.Accounts[1].Binding != nil {
		t.Fatalf("accounts = %+v, want the team account unchanged and dave's removed", out.Accounts)
	}
	if h := w.holding(t, dave); h != nil {
		t.Fatalf("dave's account still held by %s", h.Meta.Name)
	}
	if h := w.holding(t, team); h == nil || h.Spec.PolicyID != w.polB.Meta.ID {
		t.Fatalf("team account held by %+v, want polB", h)
	}

	// The same list again changes nothing.
	rec, out = w.setAccounts("dave", w.polB.Meta.ID, team)
	wantStatus(t, rec, http.StatusOK)
	if len(out.Accounts) != 1 || out.Accounts[0].PreviousBindingID != "" || out.Accounts[0].Binding == nil {
		t.Fatalf("same list = %+v, want nothing changed", out.Accounts)
	}

	// An empty list unbinds every account.
	rec, out = w.setAccounts("dave", w.polB.Meta.ID)
	wantStatus(t, rec, http.StatusOK)
	if len(out.Accounts) != 1 || !out.Accounts[0].Removed || !out.Accounts[0].PreviousBindingDeleted {
		t.Fatalf("empty list = %+v, want the team account removed", out.Accounts)
	}
	if h := w.holding(t, team); h != nil {
		t.Fatalf("team account still held by %s", h.Meta.Name)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || !strings.HasPrefix(string(raw["accounts"]), "[") {
		t.Errorf("body = %s, want an accounts array", rec.Body)
	}
}

// A refusal on any account rolls back the moves already made for the others.
func TestPolicyAccounts_RollsBackOnRefusal(t *testing.T) {
	w := newPolicyWorld(t)
	ctx := t.Context()
	// Team two's account, which dave sees as a viewer there but may not bind.
	farAcct := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "two-anthropic", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}}, Spec: account.Spec{HostID: w.host.Meta.ID}}
	must(t, "account", w.stores.Account.Upsert(ctx, farAcct))
	must(t, "reload", w.cat.Reload(ctx))

	unchanged := func(t *testing.T) {
		t.Helper()
		b := w.binding(t, w.shared.Meta.ID)
		if b == nil || !slices.Equal(b.Spec.AccountIDs, []string{w.teamAcct.Meta.ID, w.daveAcct}) || b.Spec.PolicyID != w.polA.Meta.ID {
			t.Fatalf("shared binding = %+v, want it untouched", b)
		}
		all, err := w.stores.PolicyBinding.List(ctx)
		must(t, "list", err)
		if len(all) != 1 {
			t.Fatalf("%d bindings, want only the shared one", len(all))
		}
	}

	if rec, _ := w.setAccounts("dave", w.polB.Meta.ID, w.daveAcct, farAcct.Meta.ID); rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
		t.Fatalf("another team's account: %d %s, want 403 or 400", rec.Code, rec.Body)
	}
	unchanged(t)

	if rec, _ := w.setAccounts("erin", w.polB.Meta.ID, w.daveAcct); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer's change: %d %s, want 403", rec.Code, rec.Body)
	}
	unchanged(t)

	if rec, _ := w.setAccounts("dave", w.polB.Meta.ID, ids.New()); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown account: %d %s, want 404", rec.Code, rec.Body)
	}
	unchanged(t)

	if rec, _ := w.setAccounts("carol", w.polB.Meta.ID, w.daveAcct); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger's policy: %d %s, want 404", rec.Code, rec.Body)
	}
	unchanged(t)
}
