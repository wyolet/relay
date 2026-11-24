//go:build integration

package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// secretRows counts the sealed values and the key ring's rows in every scope.
func (w *hostKeyFormWorld) secretRows(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"secret_values", "secret_data_keys", "secret_keyring"} {
		var n int
		must(t, "count "+table, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&n))
		out[table] = n
	}
	return out
}

// Every refusal that needs no write comes before the host is asked and before the value is sealed: an account the caller may not see or that is not the owner's, another host's tier, a receiver the caller may not see, a row's own grant. An organization another account holds needs the host's answer, so the host is asked once, but nothing is sealed. Real runs and dry runs alike.
func TestPseudoHostKey_RefusedBeforeProbeAndSeal(t *testing.T) {
	w := newHostKeyFormWorld(t)
	teamAcct := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "two-alpha", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}}, Spec: account.Spec{HostID: w.alpha.Meta.ID}}
	must(t, "team account", w.stores.Account.Upsert(t.Context(), teamAcct))
	must(t, "reload", w.cat.Reload(t.Context()))
	w.loadPlans(t)
	newAcct := map[string]any{"displayName": "Work"}
	for _, tc := range []struct {
		name, deny, pointer string
		spec                map[string]any
		status              int
		probes              int64
	}{
		{name: "an account the caller may not see", spec: map[string]any{"value": keyOrgC, "accountId": w.bobsAcct.Meta.ID}, status: 404, pointer: "/spec/accountId"},
		{name: "an account not the owner's", spec: map[string]any{"value": keyOrgC, "accountId": teamAcct.Meta.ID}, status: 400, pointer: "/spec/accountId"},
		{name: "another host's tier", spec: map[string]any{"value": keyOrgC, "account": newAcct, "tierPolicyId": w.betaFree.Meta.ID}, status: 400, pointer: "/spec/tierPolicyId"},
		{name: "a receiver the caller may not see", spec: map[string]any{"value": keyOrgC, "account": newAcct, "shareWith": []any{map[string]string{"kind": "team", "id": w.teamOne.Meta.ID}}}, status: 404, pointer: "/spec/shareWith/0"},
		{name: "the tier binding's grant", deny: "policy-bindings.create", spec: map[string]any{"value": keyOrgC, "account": newAcct, "tierPolicyId": w.paid.Meta.ID}, status: 403, pointer: "/spec/tierPolicyId"},
		{name: "an organization another account holds", spec: map[string]any{"value": keyOrgA, "account": newAcct}, status: 409, pointer: "/spec/account", probes: 1},
	} {
		for _, dry := range []bool{false, true} {
			*w.denied = tc.deny
			before, probes := w.secretRows(t), w.probes.Load()
			rec, _ := w.post(t, "alice", w.body("alice", tc.spec), dry)
			*w.denied = ""
			if r := refusalOf(t, rec); rec.Code != tc.status || r.Pointer != tc.pointer {
				t.Errorf("%s, dry run %v: %d %s, want %d at %s", tc.name, dry, rec.Code, rec.Body, tc.status, tc.pointer)
			}
			if diff := cmp.Diff(before, w.secretRows(t)); diff != "" {
				t.Errorf("%s, dry run %v: secret rows (-before +after):\n%s", tc.name, dry, diff)
			}
			if n := w.probes.Load() - probes; n != tc.probes {
				t.Errorf("%s, dry run %v: the host was asked %d times, want %d", tc.name, dry, n, tc.probes)
			}
		}
	}
}

// A request with a value makes one rolled-back pass before the write; a dry run's rolled-back pass is its answer; a request with no value to check or seal makes no extra pass. Counted by the reads of the plans: the rolled-back pass before a write reads them twice, a pass that answers three times.
func TestPseudoHostKey_PassesOverTheWrite(t *testing.T) {
	reads := 0
	w := newHostKeyFormWorld(t, func(d *Deps) {
		view := d.PlanView
		d.PlanView = func() *engine.View { reads++; return view() }
	})
	for _, tc := range []struct {
		name  string
		spec  map[string]any
		host  string
		dry   bool
		reads int
	}{
		{"a value", map[string]any{"value": keyOrgC, "account": map[string]any{"displayName": "Work"}}, w.alpha.Meta.ID, false, 5},
		{"a value, dry run", map[string]any{"value": keyBare, "account": map[string]any{"displayName": "Bare"}}, w.alpha.Meta.ID, true, 3},
		{"no value", map[string]any{"account": map[string]any{"displayName": "Local"}}, w.local.Meta.ID, false, 3},
	} {
		reads = 0
		body := w.body("alice", tc.spec)
		body["spec"].(map[string]any)["hostId"] = tc.host
		rec, _ := w.post(t, "alice", body, tc.dry)
		if rec.Code >= 300 || reads != tc.reads {
			t.Errorf("%s: %d %s, plans read %d times, want %d", tc.name, rec.Code, rec.Body, reads, tc.reads)
		}
		w.loadPlans(t)
	}
}

// An account held by a binding the caller may not see answers 403 as PUT /accounts/{ref}/policy does, whichever tier preset the request names, so a dry run per preset does not tell which one the hidden binding uses.
func TestPseudoHostKey_HiddenHoldingBinding(t *testing.T) {
	w := newHostKeyFormWorld(t)
	hidden := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: ids.New(), Name: "one-holds-alice-other", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}}, Spec: policybinding.Spec{PolicyID: w.free.Meta.ID, AccountIDs: []string{w.other.Meta.ID}}}
	must(t, "hidden binding", w.stores.PolicyBinding.Upsert(t.Context(), hidden))
	w.loadPlans(t)
	var answers []string
	for _, tier := range []string{w.free.Meta.ID, w.paid.Meta.ID} {
		rec, _ := w.post(t, "alice", w.body("alice", map[string]any{"value": keyOrgB, "accountId": w.other.Meta.ID, "tierPolicyId": tier}), true)
		r := refusalOf(t, rec)
		if rec.Code != http.StatusForbidden || r.Pointer != "/spec/tierPolicyId" {
			t.Errorf("tier %s: %d %s, want 403 at /spec/tierPolicyId", tier, rec.Code, rec.Body)
		}
		answers = append(answers, r.Err.Message)
	}
	want := `account "alice-other" is held by a policy binding you may not see`
	if diff := cmp.Diff([]string{want, want}, answers); diff != "" {
		t.Errorf("answers (-want +got):\n%s", diff)
	}
}

// Without a secret backend a value cannot be stored: the request answers what POST /host-keys answers then, 500 with the store's message, before the host is asked anything.
func TestPseudoHostKey_NoSecretBackend(t *testing.T) {
	w := newHostKeyFormWorld(t, func(d *Deps) {
		s := *d.Stores
		s.Stored = nil
		d.Stores = &s
	})
	for _, dry := range []bool{false, true} {
		rec, _ := w.post(t, "alice", w.body("alice", map[string]any{"value": keyOrgC, "account": map[string]any{"displayName": "Work"}}), dry)
		if r := refusalOf(t, rec); rec.Code != http.StatusInternalServerError || r.Err.Message != hostkey.ErrNoSecretBackend.Error() {
			t.Errorf("dry run %v: %d %s, want 500 %q", dry, rec.Code, rec.Body, hostkey.ErrNoSecretBackend)
		}
	}
	if n := w.probes.Load(); n != 0 {
		t.Errorf("the provider got %d requests", n)
	}
}

// A caller who may not create the host key under its owner is refused before the host is asked anything, on a dry run too: the provider gets no request.
func TestPseudoHostKey_RefusedBeforeTheProvider(t *testing.T) {
	w := newHostKeyFormWorld(t)
	for name, owner := range map[string]map[string]string{
		"no grant in the owner's scope": {"kind": "team", "id": w.teamTwo.Meta.ID},
		"a team that does not exist":    {"kind": "team", "id": ids.New()},
	} {
		for _, dry := range []bool{false, true} {
			rec, _ := w.post(t, "carol", map[string]any{
				"metadata": map[string]any{"displayName": "Probe", "owner": owner},
				"spec":     map[string]any{"hostId": w.alpha.Meta.ID, "value": keyOrgA, "account": map[string]any{"displayName": "Probe"}},
			}, dry)
			if r := refusalOf(t, rec); rec.Code != http.StatusForbidden || r.Pointer != "/metadata/owner" {
				t.Errorf("%s, dry run %v: %d %s, want 403 at /metadata/owner", name, dry, rec.Code, rec.Body)
			}
		}
	}
	if n := w.probes.Load(); n != 0 {
		t.Errorf("the provider got %d requests from callers refused", n)
	}
}

// A caller who may not create the host key under its owner, or an owner a host key cannot have, is refused before the value is sealed: no sealed value and no data key is written for any scope.
func TestPseudoHostKey_RefusedBeforeSealing(t *testing.T) {
	w := newHostKeyFormWorld(t)
	for _, tc := range []struct {
		name   string
		owner  map[string]string
		status int
	}{
		{"a team that does not exist", map[string]string{"kind": "team", "id": ids.New()}, http.StatusForbidden},
		{"a host", map[string]string{"kind": "host", "id": w.alpha.Meta.ID}, http.StatusBadRequest},
		{"a provider", map[string]string{"kind": "provider", "id": ids.New()}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := w.secretRows(t)
			for _, dry := range []bool{false, true} {
				rec, _ := w.post(t, "carol", map[string]any{
					"metadata": map[string]any{"displayName": "Probe", "owner": tc.owner},
					"spec":     map[string]any{"hostId": w.alpha.Meta.ID, "value": keyOrgA, "account": map[string]any{"displayName": "Probe"}},
				}, dry)
				if r := refusalOf(t, rec); rec.Code != tc.status || r.Pointer != "/metadata/owner" {
					t.Errorf("dry run %v: %d %s, want %d at /metadata/owner", dry, rec.Code, rec.Body, tc.status)
				}
			}
			if diff := cmp.Diff(before, w.secretRows(t)); diff != "" {
				t.Errorf("secret rows (-before +after):\n%s", diff)
			}
		})
	}
}
