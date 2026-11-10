//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/ids"
)

// A host key names its provider account on create and on update; one that names none is refused and no account appears for it.
func TestIntegration_HostKeyNeedsAnAccount(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	system := meta.Owner{Kind: meta.OwnerSystem}
	h := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "needs-account", Owner: system}, Spec: host.Spec{BaseURL: "https://needs-account.example.com"}}
	mustUpsert(t, st.stores.Host.Upsert(ctx, h), "host")
	code := func(raw []byte) string {
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &env)
		return env.Error.Code
	}

	body := `{"metadata":{"name":"no-account-key"},"spec":{"hostId":"` + h.Meta.ID + `","valueFrom":{"kind":"env","env":"NEEDS_ACCOUNT_KEY"}}}`
	status, raw := st.adminDo(http.MethodPost, "/api/host-keys", body)
	if status != http.StatusBadRequest || code(raw) != "account_required" || !strings.Contains(string(raw), "create one at this host first") {
		t.Fatalf("create without an account = %d %s, want 400 account_required", status, raw)
	}
	if all, _ := st.stores.Account.List(ctx); len(all) != 0 {
		for _, a := range all {
			if a.IsProvider() {
				t.Fatalf("a refused key left provider account %q behind", a.Meta.Name)
			}
		}
	}

	acct := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "needs-account-org", Owner: system}, Spec: account.Spec{HostID: h.Meta.ID}}
	mustUpsert(t, st.stores.Account.Upsert(ctx, acct), "account")
	body = `{"metadata":{"name":"with-account-key"},"spec":{"hostId":"` + h.Meta.ID + `","accountId":"` + acct.Meta.ID + `","valueFrom":{"kind":"env","env":"NEEDS_ACCOUNT_KEY"}}}`
	status, raw = st.adminDo(http.MethodPost, "/api/host-keys", body)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create with an account = %d %s", status, raw)
	}
	var created struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.Metadata.ID == "" {
		t.Fatalf("created key: %s (%v)", raw, err)
	}

	body = `{"metadata":{"name":"with-account-key"},"spec":{"hostId":"` + h.Meta.ID + `","valueFrom":{"kind":"env","env":"NEEDS_ACCOUNT_KEY"}}}`
	status, raw = st.adminDo(http.MethodPut, "/api/host-keys/by-id/"+created.Metadata.ID, body)
	if status != http.StatusBadRequest || code(raw) != "account_required" {
		t.Fatalf("update without an account = %d %s, want 400 account_required", status, raw)
	}
}

// A provider account is an ordinary row: one no key names still keeps its team from being deleted, as any row the team owns does, grouped by relay or not.
func TestIntegration_KeylessAccountsBlockTheirOwner(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	system := meta.Owner{Kind: meta.OwnerSystem}
	h := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "keyless-accounts", Owner: system}, Spec: host.Spec{BaseURL: "https://keyless-accounts.example.com"}}
	mustUpsert(t, st.stores.Host.Upsert(ctx, h), "host")
	teamID := st.mkTeam(t, "keyless-accounts")
	acct := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "keyless-team-org", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}, Spec: account.Spec{HostID: h.Meta.ID}}
	mustUpsert(t, st.stores.Account.Upsert(ctx, acct), "account")
	if _, err := testPool(t, st.dsn).Exec(ctx, `UPDATE accounts SET grouped_by_relay = TRUE WHERE id = $1`, acct.Meta.ID); err != nil {
		t.Fatal(err)
	}
	status, raw := st.adminDo(http.MethodDelete, "/api/teams/by-id/"+teamID, "")
	if status != http.StatusConflict || !strings.Contains(string(raw), `"account"`) {
		t.Fatalf("DELETE team = %d %s, want 409 naming the account", status, raw)
	}
	if a, err := st.stores.Account.Get(ctx, acct.Meta.ID); err != nil || a == nil {
		t.Fatalf("account after the refused delete = %v, %v", a, err)
	}
}
