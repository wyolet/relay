//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
)

// adminSend sends body to path as the admin token.
func (f policyDeleteFixture) adminSend(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-admin-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

// Accounts, pools and budgets go through the generic CRUD: a create fills
// the defaults the manifest form omits, and the cross-row rules refuse a
// pool holding another host's key.
func TestUpstreamKinds_CRUD(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	hk := f.hostKey(t, ctx, "crud-key")
	other := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "crud-other-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://other.example.com"}}
	must(t, "upsert host", f.stores.Host.Upsert(ctx, other))

	w := f.adminSend(t, http.MethodPost, "/accounts", `{"metadata":{"name":"crud-org"},"spec":{"hostId":"`+f.host.Meta.ID+`","orgId":"org-1"}}`)
	wantStatus(t, w, http.StatusCreated)
	var acct struct {
		Metadata meta.Metadata `json:"metadata"`
	}
	must(t, "decode account", json.Unmarshal(w.Body.Bytes(), &acct))

	w = f.adminSend(t, http.MethodPost, "/pools", `{"metadata":{"name":"crud-pool"},"spec":{"hostId":"`+f.host.Meta.ID+`","credentialIds":["`+hk.Meta.ID+`"]}}`)
	wantStatus(t, w, http.StatusCreated)
	var pl struct {
		Spec struct{ Selection string } `json:"spec"`
	}
	must(t, "decode pool", json.Unmarshal(w.Body.Bytes(), &pl))
	if pl.Spec.Selection != "prioritized" {
		t.Errorf("pool selection = %q, want the default prioritized", pl.Spec.Selection)
	}

	w = f.adminSend(t, http.MethodPost, "/pools", `{"metadata":{"name":"crud-cross-pool"},"spec":{"hostId":"`+other.Meta.ID+`","credentialIds":["`+hk.Meta.ID+`"]}}`)
	wantStatus(t, w, http.StatusBadRequest)

	w = f.adminSend(t, http.MethodPost, "/budgets", `{"metadata":{"name":"crud-budget"},"spec":{"amountUsd":"12.50"}}`)
	wantStatus(t, w, http.StatusCreated)
	var bd struct {
		Spec struct{ Period, Mode string } `json:"spec"`
	}
	must(t, "decode budget", json.Unmarshal(w.Body.Bytes(), &bd))
	if bd.Spec.Period != "month" || bd.Spec.Mode != "hard" {
		t.Errorf("budget spec = %+v, want month / hard defaults", bd.Spec)
	}

	// A host key naming the account holds it; the host holds the pool.
	hk.Spec.AccountID = acct.Metadata.ID
	must(t, "upsert host key", f.stores.HostKey.Upsert(ctx, hk))
	body := decodeInUse(t, f.deleteRow(t, "accounts", acct.Metadata.ID, false))
	if len(body.Blockers) != 1 || body.Blockers[0].Kind != "host-key" {
		t.Fatalf("account delete blockers = %+v, want the host key", body.Blockers)
	}
	body = decodeInUse(t, f.deleteRow(t, "hosts", f.host.Meta.ID, false))
	kinds := map[string]bool{}
	for _, g := range body.Blockers {
		kinds[g.Kind] = true
	}
	if !kinds["pool"] || !kinds["account"] {
		t.Fatalf("host delete blockers = %+v, want the pool and the account among them", body.Blockers)
	}

	w = f.adminSend(t, http.MethodGet, "/accounts/crud-org/keys", "")
	wantStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"name":"crud-key"`) {
		t.Fatalf("account keys = %s, want crud-key", w.Body)
	}
	w = f.adminSend(t, http.MethodGet, "/pools/crud-pool/keys", "")
	wantStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"name":"crud-key"`) {
		t.Fatalf("pool keys = %s, want crud-key", w.Body)
	}

	// Deleting the key narrows the pool and reports it.
	w = f.deleteRow(t, "host-keys", hk.Meta.ID, false)
	wantStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"kind":"pool"`) {
		t.Fatalf("host key delete answer = %s, want the pool reported", w.Body)
	}
	if got, err := f.stores.HostKey.Get(ctx, hk.Meta.ID); err != nil || got != (*hostkey.HostKey)(nil) {
		t.Fatalf("host key after delete = %+v, %v", got, err)
	}
}

// A host-key PUT that omits accountId keeps the key in its account, as an omitted secret is kept: a client unaware of accounts must not move a key out of its provider org.
func TestHostKeyUpdateKeepsItsAccount(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	hk := f.hostKey(t, ctx, "kept-key")
	w := f.adminSend(t, http.MethodPost, "/accounts", `{"metadata":{"name":"kept-org"},"spec":{"hostId":"`+f.host.Meta.ID+`"}}`)
	wantStatus(t, w, http.StatusCreated)
	var acct struct {
		Metadata meta.Metadata `json:"metadata"`
	}
	must(t, "decode account", json.Unmarshal(w.Body.Bytes(), &acct))
	hk.Spec.AccountID = acct.Metadata.ID
	must(t, "upsert host key", f.stores.HostKey.Upsert(ctx, hk))

	w = f.adminSend(t, http.MethodPut, "/host-keys/by-id/"+hk.Meta.ID, `{"metadata":{"name":"kept-key","displayName":"Renamed"},"spec":{"hostId":"`+f.host.Meta.ID+`","valueFrom":{"kind":"env","env":"DEL_HOSTKEY_VALUE"}}}`)
	wantStatus(t, w, http.StatusOK)
	got, err := f.stores.HostKey.Get(ctx, hk.Meta.ID)
	must(t, "get host key", err)
	if got.Meta.DisplayName != "Renamed" || got.Spec.AccountID != acct.Metadata.ID {
		t.Fatalf("after a PUT without accountId: displayName %q, account %q; want Renamed in %q", got.Meta.DisplayName, got.Spec.AccountID, acct.Metadata.ID)
	}
}

// A project's host key may not join a system account: the account's counters and 429 cooldown are the system keys' too. Its own project's account is fine.
func TestHostKeyJoinsOnlyItsOwnersAccount(t *testing.T) {
	f, _ := newPolicyDeleteFixture(t)
	t.Setenv("JOIN_HOSTKEY_VALUE", "sk-join")
	account := func(name, owner string) string {
		w := f.adminSend(t, http.MethodPost, "/accounts", `{"metadata":{"name":"`+name+`"`+owner+`},"spec":{"hostId":"`+f.host.Meta.ID+`"}}`)
		wantStatus(t, w, http.StatusCreated)
		var acct struct {
			Metadata meta.Metadata `json:"metadata"`
		}
		must(t, "decode account", json.Unmarshal(w.Body.Bytes(), &acct))
		return acct.Metadata.ID
	}
	inProject := `,"owner":{"kind":"project","id":"` + f.proj.Meta.ID + `"}`
	shared, own := account("join-shared", ""), account("join-own", inProject)
	key := func(name, accountID string) string {
		return `{"metadata":{"name":"` + name + `"` + inProject + `},"spec":{"hostId":"` + f.host.Meta.ID + `","accountId":"` + accountID + `","valueFrom":{"kind":"env","env":"JOIN_HOSTKEY_VALUE"}}}`
	}
	w := f.adminSend(t, http.MethodPost, "/host-keys", key("join-shared-key", shared))
	wantStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "its own owner") {
		t.Fatalf("refusal = %s, want the account-owner rule", w.Body.String())
	}
	w = f.adminSend(t, http.MethodPost, "/host-keys", key("join-own-key", own))
	wantStatus(t, w, http.StatusCreated)
}
