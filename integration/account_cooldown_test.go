//go:build integration

package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/ids"
)

// cooldownStack serves "cooldown-model" at one host whose upstream answers its first call 429 (Retry-After 30 s) and every later one 200; a user's key reaches the host's system-owned credentials by ownership.
type cooldownStack struct {
	*stack
	hostID string
	bearer string
	mu     sync.Mutex
	calls  int
}

func newCooldownStack(t *testing.T) *cooldownStack {
	t.Helper()
	cs := &cooldownStack{stack: newStack(t)}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.calls++
		first := cs.calls == 1
		cs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"cooldown-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(up.Close)

	ctx := context.Background()
	system := meta.Owner{Kind: meta.OwnerSystem}
	prov := &provider.Provider{Meta: meta.Metadata{ID: ids.New(), Name: "cooldown-prov", Owner: system}}
	mustUpsert(t, cs.stores.Provider.Upsert(ctx, prov), "provider")
	hst := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "cooldown-host", Owner: system}, Spec: host.Spec{BaseURL: up.URL}}
	mustUpsert(t, cs.stores.Host.Upsert(ctx, hst), "host")
	cs.hostID = hst.Meta.ID
	mdl := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "cooldown-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "cooldown-model"}}, Pointer: "cooldown-model"},
	}
	mustUpsert(t, cs.stores.Model.Upsert(ctx, mdl), "model")
	mustUpsert(t, cs.stores.Binding.Upsert(ctx, &binding.Binding{
		Meta: meta.Metadata{ID: ids.New(), Name: "cooldown-model-on-host", Owner: system},
		Spec: binding.Spec{ModelID: mdl.Meta.ID, HostID: hst.Meta.ID, Adapter: adapters.OpenAI},
	}), "binding")
	userID := cs.seedUser("cooldown-user-" + ids.New()[:8])
	cs.bearer = "rk_cooldown_" + ids.New()
	mustUpsert(t, cs.stores.Key.Upsert(ctx, &key.Key{
		Meta: meta.Metadata{ID: ids.New(), Name: "cooldown-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: userID}, KeyHash: sha256Hex(cs.bearer), Prefix: "rk_cooldown"},
	}), "key")
	return cs
}

// credentialIn stores a system-owned env credential at the host in accountID and returns that account.
func (cs *cooldownStack) credentialIn(t *testing.T, name, accountID string) string {
	t.Helper()
	env := uniqueEnvName("COOLDOWN_")
	if err := os.Setenv(env, "sk-"+name); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(env) })
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: cs.hostID, AccountID: accountID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: env}},
	}
	mustUpsert(t, cs.stores.HostKey.Upsert(context.Background(), hk), "host key")
	return hk.Spec.AccountID
}

func (cs *cooldownStack) account(t *testing.T, name string) string {
	t.Helper()
	a := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: cs.hostID}}
	mustUpsert(t, cs.stores.Account.Upsert(context.Background(), a), "account")
	return a.Meta.ID
}

func (cs *cooldownStack) sync(t *testing.T) {
	t.Helper()
	if err := cs.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

func (cs *cooldownStack) chat(t *testing.T) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, cs.inference.URL+"/v1/chat/completions", strings.NewReader(`{"model":"cooldown-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cs.bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

func (cs *cooldownStack) upstreamCalls() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.calls
}

// An account the upgrade grouped by owner and host does not declare one provider org: a provider 429 on one of its keys trips that key's breaker and the request fails over to the sibling.
func TestIntegration_Upstream429InGroupedAccountFailsOver(t *testing.T) {
	t.Parallel()
	cs := newCooldownStack(t)
	grouped := cs.account(t, "cooldown-grouped")
	ctx := context.Background()
	if _, err := testPool(t, cs.dsn).Exec(ctx, `UPDATE accounts SET grouped_by_relay = TRUE WHERE id = $1`, grouped); err != nil {
		t.Fatal(err)
	}
	cs.credentialIn(t, "cooldown-key-a", grouped)
	cs.credentialIn(t, "cooldown-key-b", grouped)
	acc, err := cs.stores.Account.Get(ctx, grouped)
	if err != nil || !acc.Spec.GroupedByRelay {
		t.Fatalf("account %q = %+v, %v; want it grouped by relay", grouped, acc, err)
	}
	cs.sync(t)

	code, _, raw := cs.chat(t)
	if code != http.StatusOK {
		t.Fatalf("chat = %d %s, want 200 from the second key", code, raw)
	}
	if n := cs.upstreamCalls(); n != 2 {
		t.Errorf("upstream calls = %d, want 2", n)
	}
}

// Keys in one explicit account share its capacity: a provider 429 on one cools the account for the model, so the caller gets the 429 and the sibling is not tried.
func TestIntegration_Upstream429CoolsExplicitAccount(t *testing.T) {
	t.Parallel()
	cs := newCooldownStack(t)
	org := cs.account(t, "cooldown-org")
	cs.credentialIn(t, "cooldown-key-a", org)
	cs.credentialIn(t, "cooldown-key-b", org)
	cs.sync(t)

	code, hdr, raw := cs.chat(t)
	if code != http.StatusTooManyRequests {
		t.Fatalf("chat = %d %s, want 429 with the account cooling", code, raw)
	}
	if hdr.Get("Retry-After") == "" {
		t.Errorf("429 carries no Retry-After")
	}
	if n := cs.upstreamCalls(); n != 1 {
		t.Errorf("upstream calls = %d, want 1: the sibling in the same account must be skipped", n)
	}
}

// Keys in separate accounts keep separate capacity: a 429 on one fails over to the other.
func TestIntegration_Upstream429FailsOverAcrossAccounts(t *testing.T) {
	t.Parallel()
	cs := newCooldownStack(t)
	cs.credentialIn(t, "cooldown-key-a", cs.account(t, "cooldown-org-a"))
	cs.credentialIn(t, "cooldown-key-b", cs.account(t, "cooldown-org-b"))
	cs.sync(t)

	code, _, raw := cs.chat(t)
	if code != http.StatusOK {
		t.Fatalf("chat = %d %s, want 200 from the second account", code, raw)
	}
	if n := cs.upstreamCalls(); n != 2 {
		t.Errorf("upstream calls = %d, want 2", n)
	}
}
