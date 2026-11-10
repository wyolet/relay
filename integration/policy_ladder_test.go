//go:build integration

// policy_ladder_test.go drives the policy gate end to end: links made and removed through the control API, and the inference answers that follow, including after a reload from the store.

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/ids"
)

// ladderStack is a stack serving "ladder-model" (one USD per token, so each answer of one input and one output token costs two dollars) at a host whose upstream records the credential each call used, a system-owned credential every principal reaches by ownership, and a user's key.
type ladderStack struct {
	*stack
	hostID    string
	accountID string // the system provider account every credential() joins
	userID    string
	bearer    string
	systemKey string
	mu        sync.Mutex
	used      []string
}

func newLadderStack(t *testing.T) *ladderStack {
	t.Helper()
	ls := &ladderStack{stack: newStack(t)}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		ls.used = append(ls.used, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		ls.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"ladder-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(up.Close)

	ctx := context.Background()
	system := meta.Owner{Kind: meta.OwnerSystem}
	prov := &provider.Provider{Meta: meta.Metadata{ID: ids.New(), Name: "ladder-prov", Owner: system}}
	mustUpsert(t, ls.stores.Provider.Upsert(ctx, prov), "provider")
	hst := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "ladder-host", Owner: system}, Spec: host.Spec{BaseURL: up.URL}}
	mustUpsert(t, ls.stores.Host.Upsert(ctx, hst), "host")
	ls.hostID = hst.Meta.ID
	ls.accountID = providerAccount(t, ls.stores, "ladder-account", system, hst.Meta.ID)
	mdl := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "ladder-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "ladder-model"}}, Pointer: "ladder-model"},
	}
	mustUpsert(t, ls.stores.Model.Upsert(ctx, mdl), "model")
	mustUpsert(t, ls.stores.Binding.Upsert(ctx, &binding.Binding{
		Meta: meta.Metadata{ID: ids.New(), Name: "ladder-model-on-host", Owner: system},
		Spec: binding.Spec{ModelID: mdl.Meta.ID, HostID: hst.Meta.ID, Adapter: adapters.OpenAI},
	}), "binding")
	mustUpsert(t, ls.stores.Pricing.Upsert(ctx, &pricing.Pricing{
		Meta: meta.Metadata{ID: ids.New(), Name: "ladder-model-price", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}},
		Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{mdl.Meta.ID}, Rates: []pricing.Rate{
			{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1_000_000},
			{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: 1_000_000},
		}},
	}), "pricing")
	ls.systemKey = ls.credential(t, "ladder-system-key")
	ls.userID = ls.seedUser("ladder-user-" + ids.New()[:8])
	ls.bearer = "rk_ladder_" + ids.New()
	mustUpsert(t, ls.stores.Key.Upsert(ctx, &key.Key{
		Meta: meta.Metadata{ID: ids.New(), Name: "ladder-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: ls.userID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: ls.userID}, KeyHash: sha256Hex(ls.bearer), Prefix: "rk_ladder"},
	}), "key")
	ls.sync(t)
	return ls
}

// credential stores a system-owned env credential at the host and returns its value.
func (ls *ladderStack) credential(t *testing.T, name string) string {
	t.Helper()
	value := "sk-" + name
	env := uniqueEnvName("LADDER_")
	if err := os.Setenv(env, value); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(env) })
	mustUpsert(t, ls.stores.HostKey.Upsert(context.Background(), &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: ls.hostID, AccountID: ls.accountID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: env}},
	}), "host key")
	return value
}

func (ls *ladderStack) credentialID(t *testing.T, name string) string {
	t.Helper()
	keys, err := ls.stores.HostKey.List(context.Background())
	if err != nil {
		t.Fatalf("list host keys: %v", err)
	}
	for _, k := range keys {
		if k.Meta.Name == name {
			return k.Meta.ID
		}
	}
	t.Fatalf("no host key %s", name)
	return ""
}

func (ls *ladderStack) sync(t *testing.T) {
	t.Helper()
	if err := ls.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

// create POSTs body to the control API and returns the new row's id.
func (ls *ladderStack) create(t *testing.T, plural, body string) string {
	t.Helper()
	code, raw := ls.adminDo(http.MethodPost, "/api/"+plural, body)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/%s = %d: %s", plural, code, raw)
	}
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode %s: %v", plural, err)
	}
	return row.Metadata.ID
}

// bindToUser binds policyID to the stack's user's personal account, the key's lane, from the system scope.
func (ls *ladderStack) bindToUser(t *testing.T, name, policyID string) {
	t.Helper()
	ls.create(t, "policy-bindings", `{"metadata":{"name":"`+name+`","owner":{"kind":"system"}},"spec":{"policyId":"`+policyID+`","accountIds":["`+ls.accountAt(t, "personal:"+ls.userID)+`"]}}`)
}

func (ls *ladderStack) chat(t *testing.T) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ls.inference.URL+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"ladder-model","messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ls.bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

// chatUntil sends requests until one answers want, for refusals that land once a detached post-flight has settled.
func (ls *ladderStack) chatUntil(t *testing.T, want int) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, _, raw := ls.chat(t)
		if code == want {
			return raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("never answered %d; last %d %s", want, code, raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (ls *ladderStack) lastUsed() string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.used) == 0 {
		return ""
	}
	return ls.used[len(ls.used)-1]
}

func (ls *ladderStack) calls() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.used)
}

// deleteReportsPolicy deletes a row and checks the answer reports the policy that lost its link.
func (ls *ladderStack) deleteReportsPolicy(t *testing.T, plural, id, policyName string) {
	t.Helper()
	code, raw := ls.adminDo(http.MethodDelete, "/api/"+plural+"/by-id/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /api/%s = %d: %s, want 200 with the detached links", plural, code, raw)
	}
	var body struct {
		Detached []struct {
			Kind  string `json:"kind"`
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		} `json:"detached"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode delete answer %s: %v", raw, err)
	}
	for _, g := range body.Detached {
		for _, it := range g.Items {
			if it.Name == policyName {
				return
			}
		}
	}
	t.Fatalf("delete answer %s does not report policy %s", raw, policyName)
}

func (ls *ladderStack) policySpec(t *testing.T, id string) map[string]any {
	t.Helper()
	code, raw := ls.adminDo(http.MethodGet, "/api/policies/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET policy = %d: %s", code, raw)
	}
	var row struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return row.Spec
}

// A hard budget refuses with 429 once spent; deleting it is allowed, the answer reports the policy that loses the cap, and the policy then admits again.
func TestIntegration_DeletingALinkedBudgetReportsAndLiftsTheCap(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	budgetID := ls.create(t, "budgets", `{"metadata":{"name":"ladder-budget"},"spec":{"amountUsd":"1"}}`)
	policyID := ls.create(t, "policies", `{"metadata":{"name":"ladder-budgeted"},"spec":{"budgets":[{"budgetId":"`+budgetID+`"}]}}`)
	ls.bindToUser(t, "ladder-budgeted-user", policyID)
	ls.sync(t)

	if code, _, raw := ls.chat(t); code != http.StatusOK {
		t.Fatalf("first request = %d %s", code, raw)
	}
	raw := ls.chatUntil(t, http.StatusTooManyRequests)
	if !strings.Contains(string(raw), "budget_exceeded") {
		t.Fatalf("refusal %s, want budget_exceeded", raw)
	}

	ls.deleteReportsPolicy(t, "budgets", budgetID, "ladder-budgeted")
	if ids, _ := ls.policySpec(t, policyID)["budgets"].([]any); len(ids) != 0 {
		t.Fatalf("policy still links budgets %v after the delete", ids)
	}
	ls.sync(t)
	if code, _, raw := ls.chat(t); code != http.StatusOK {
		t.Fatalf("after the budget delete = %d %s, want the policy to admit without the cap", code, raw)
	}
}

// Deleting a rate limit a policy links is allowed and reported; the policy keeps its other links and stops counting that limit.
func TestIntegration_DeletingALinkedRateLimitReportsAndLiftsTheCap(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	rlID := ls.create(t, "rate-limits", `{"metadata":{"name":"ladder-one-an-hour"},"spec":{"rules":[{"meter":"requests","amount":1,"window":3600,"strategy":"fixed-window"}]}}`)
	budgetID := ls.create(t, "budgets", `{"metadata":{"name":"ladder-roomy"},"spec":{"amountUsd":"1000"}}`)
	policyID := ls.create(t, "policies", `{"metadata":{"name":"ladder-limited"},"spec":{"rateLimits":[{"rateLimitId":"`+rlID+`"}],"budgets":[{"budgetId":"`+budgetID+`"}]}}`)
	ls.bindToUser(t, "ladder-limited-user", policyID)
	ls.sync(t)

	if code, _, raw := ls.chat(t); code != http.StatusOK {
		t.Fatalf("first request = %d %s", code, raw)
	}
	if code, hdr, raw := ls.chat(t); code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" {
		t.Fatalf("second request = %d (Retry-After %q) %s, want 429 with Retry-After", code, hdr.Get("Retry-After"), raw)
	}

	ls.deleteReportsPolicy(t, "rate-limits", rlID, "ladder-limited")
	spec := ls.policySpec(t, policyID)
	if rls, _ := spec["rateLimits"].([]any); len(rls) != 0 {
		t.Fatalf("policy still links rate limits %v after the delete", rls)
	}
	if bs, _ := spec["budgets"].([]any); len(bs) != 1 {
		t.Fatalf("policy budgets = %v, want the budget kept", bs)
	}
	ls.sync(t)
	for range 2 {
		if code, _, raw := ls.chat(t); code != http.StatusOK {
			t.Fatalf("after the rate-limit delete = %d %s", code, raw)
		}
	}
}

// Deleting the pool a policy links is allowed and reported; the policy narrows to nothing rather than falling back to the system credential, and stays narrow after a reload from the store.
func TestIntegration_DeletingALinkedPoolNarrowsNeverWidens(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	pooled := ls.credential(t, "ladder-pooled-key")
	poolID := ls.create(t, "pools", `{"metadata":{"name":"ladder-pool"},"spec":{"hostId":"`+ls.hostID+`","credentialIds":["`+ls.credentialID(t, "ladder-pooled-key")+`"]}}`)
	policyID := ls.create(t, "policies", `{"metadata":{"name":"ladder-pooled"},"spec":{"poolIds":["`+poolID+`"]}}`)
	ls.bindToUser(t, "ladder-pooled-user", policyID)
	ls.sync(t)

	if code, _, raw := ls.chat(t); code != http.StatusOK || ls.lastUsed() != pooled {
		t.Fatalf("before the delete = %d %s using %q, want the pooled credential", code, raw, ls.lastUsed())
	}
	ls.deleteReportsPolicy(t, "pools", poolID, "ladder-pooled")
	ls.sync(t)
	before := ls.calls()
	if code, _, raw := ls.chat(t); code == http.StatusOK {
		t.Fatalf("after the pool delete = %d %s using %q, want a refusal (never the system credential)", code, raw, ls.lastUsed())
	}
	if ls.calls() != before {
		t.Fatal("the refused request reached the upstream")
	}
}

// Deleting a credential a policy's pool holds narrows the pool to its other credentials; the policy never widens to ownership.
func TestIntegration_DeletingAPooledCredentialNarrows(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	ls.credential(t, "ladder-direct-a")
	kept := ls.credential(t, "ladder-direct-b")
	a, b := ls.credentialID(t, "ladder-direct-a"), ls.credentialID(t, "ladder-direct-b")
	poolID := ls.create(t, "pools", `{"metadata":{"name":"ladder-direct-pool"},"spec":{"hostId":"`+ls.hostID+`","credentialIds":["`+a+`","`+b+`"]}}`)
	policyID := ls.create(t, "policies", `{"metadata":{"name":"ladder-direct"},"spec":{"poolIds":["`+poolID+`"]}}`)
	ls.bindToUser(t, "ladder-direct-user", policyID)
	ls.sync(t)

	code, raw := ls.adminDo(http.MethodDelete, "/api/host-keys/by-id/"+a, "")
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("DELETE a pooled credential = %d %s", code, raw)
	}
	ls.sync(t)
	for range 3 {
		if code, _, raw := ls.chat(t); code != http.StatusOK || ls.lastUsed() != kept {
			t.Fatalf("after deleting one pooled credential = %d %s using %q, want only %q", code, raw, ls.lastUsed(), kept)
		}
	}
}

// An account holds at most one policy: a second binding naming the same account is refused on save.
func TestIntegration_SecondBindingOnOneAccountIsRefused(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	first := ls.create(t, "policies", `{"metadata":{"name":"ladder-first"},"spec":{}}`)
	second := ls.create(t, "policies", `{"metadata":{"name":"ladder-second"},"spec":{}}`)
	ls.bindToUser(t, "ladder-first-user", first)
	personal := ls.accountAt(t, "personal:"+ls.userID)
	code, raw := ls.adminDo(http.MethodPost, "/api/policy-bindings", `{"metadata":{"name":"ladder-second-user","owner":{"kind":"system"}},"spec":{"policyId":"`+second+`","accountIds":["`+personal+`"]}}`)
	if code != http.StatusConflict {
		t.Fatalf("second binding on the personal account = %d %s, want 409", code, raw)
	}

	teamID := ls.mkTeam(t, "ladder-dup-team")
	team := ls.accountAt(t, "team:"+teamID)
	if code, raw := ls.adminDo(http.MethodPost, "/api/policy-bindings", `{"metadata":{"name":"ladder-team-a","owner":{"kind":"system"}},"spec":{"policyId":"`+first+`","accountIds":["`+team+`"]}}`); code != http.StatusCreated {
		t.Fatalf("first binding on the team account = %d %s", code, raw)
	}
	if code, raw := ls.adminDo(http.MethodPost, "/api/policy-bindings", `{"metadata":{"name":"ladder-team-b","owner":{"kind":"team","id":"`+teamID+`"}},"spec":{"policyId":"`+second+`","accountIds":["`+team+`"]}}`); code != http.StatusConflict {
		t.Fatalf("second binding on the team account = %d %s, want 409", code, raw)
	}
}

// A team binds a host-owned tier preset to its own provider account and not to another team's or the system's; the system binds any.
func TestIntegration_BindingOwnersForProviderAccounts(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	teamID := ls.mkTeam(t, "ladder-owners-team")
	otherTeam := ls.mkTeam(t, "ladder-owners-other")
	ownTeam := `{"kind":"team","id":"` + teamID + `"}`
	accountID := ls.create(t, "accounts", `{"metadata":{"name":"ladder-team-org","owner":`+ownTeam+`},"spec":{"hostId":"`+ls.hostID+`"}}`)
	otherAccount := ls.create(t, "accounts", `{"metadata":{"name":"ladder-other-org","owner":{"kind":"team","id":"`+otherTeam+`"}},"spec":{"hostId":"`+ls.hostID+`"}}`)
	// The API refuses a system owner on create, so the system's account is written to the store.
	systemOrg := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "ladder-system-org", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: ls.hostID}}
	mustUpsert(t, ls.stores.Account.Upsert(context.Background(), systemOrg), "account")
	systemAccount := systemOrg.Meta.ID
	preset := ls.create(t, "policies", `{"metadata":{"name":"ladder-tier-1","owner":{"kind":"host","id":"`+ls.hostID+`"}},"spec":{"models":["ladder-prov/ladder-model"]}}`)
	binding := func(name, owner, accountID string) string {
		return `{"metadata":{"name":"` + name + `","owner":` + owner + `},"spec":{"policyId":"` + preset + `","accountIds":["` + accountID + `"]}}`
	}

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"a team binding naming a system account", binding("ladder-team-system", ownTeam, systemAccount), http.StatusBadRequest},
		{"a team binding naming another team's account", binding("ladder-team-foreign", ownTeam, otherAccount), http.StatusBadRequest},
		{"a team binds a tier preset to its own account", binding("ladder-team-tier", ownTeam, accountID), http.StatusCreated},
		{"the system binds another team's account", binding("ladder-system-foreign", `{"kind":"system"}`, otherAccount), http.StatusCreated},
	} {
		if code, raw := ls.adminDo(http.MethodPost, "/api/policy-bindings", tc.body); code != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, code, raw, tc.want)
		}
	}
}

// Every object a policy links may be owned by a team: a team owns its rate limits and credentials beside its pools, budgets and accounts, and a team policy links them.
func TestIntegration_TeamOwnsEveryLinkableKind(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	teamID := ls.mkTeam(t, "ladder-owning-team")
	owner := `{"kind":"team","id":"` + teamID + `"}`
	env := uniqueEnvName("LADDER_TEAM_")
	if err := os.Setenv(env, "sk-team-owned"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(env) })

	created := map[string]string{}
	for _, tc := range []struct{ plural, body string }{
		{"budgets", `{"metadata":{"name":"ladder-team-budget","owner":` + owner + `},"spec":{"amountUsd":"5"}}`},
		{"accounts", `{"metadata":{"name":"ladder-team-account","owner":` + owner + `},"spec":{"hostId":"` + ls.hostID + `"}}`},
		{"rate-limits", `{"metadata":{"name":"ladder-team-limit","owner":` + owner + `},"spec":{"rules":[{"meter":"requests","amount":5,"window":60,"strategy":"fixed-window"}]}}`},
		{"host-keys", `{"metadata":{"name":"ladder-team-key","owner":` + owner + `},"spec":{"hostId":"` + ls.hostID + `","accountId":"{account}","valueFrom":{"kind":"env","env":"` + env + `"}}}`},
	} {
		// The host key joins the team's account created above.
		code, raw := ls.adminDo(http.MethodPost, "/api/"+tc.plural, strings.ReplaceAll(tc.body, "{account}", created["accounts"]))
		if code != http.StatusCreated {
			t.Errorf("team-owned %s = %d %s, want 201", tc.plural, code, raw)
			continue
		}
		var row tenancyRow
		_ = json.Unmarshal(raw, &row)
		created[tc.plural] = row.Metadata.ID
	}
	if t.Failed() {
		return
	}
	poolID := ls.create(t, "pools", `{"metadata":{"name":"ladder-team-pool","owner":`+owner+`},"spec":{"hostId":"`+ls.hostID+`","credentialIds":["`+created["host-keys"]+`"]}}`)
	ls.create(t, "policies", `{"metadata":{"name":"ladder-team-policy","owner":`+owner+`},"spec":{"poolIds":["`+poolID+`"],"rateLimits":[{"rateLimitId":"`+created["rate-limits"]+`"}],"budgets":[{"budgetId":"`+created["budgets"]+`"}]}}`)
}

// Upstream-rung (host-owned tier) policies carry models, limits and budgets, never pools.
func TestIntegration_TierPolicyLinksNoPools(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	poolID := ls.create(t, "pools", `{"metadata":{"name":"ladder-tier-pool"},"spec":{"hostId":"`+ls.hostID+`"}}`)
	rlID := ls.create(t, "rate-limits", `{"metadata":{"name":"ladder-tier-limit"},"spec":{"rules":[{"meter":"requests","amount":5,"window":60,"strategy":"fixed-window"}]}}`)
	owner := `{"kind":"host","id":"` + ls.hostID + `"}`
	for _, tc := range []struct {
		name, spec string
		want       int
	}{
		{"pools", `{"poolIds":["` + poolID + `"]}`, http.StatusBadRequest},
		{"limits", `{"models":["ladder-prov/ladder-model"],"rateLimits":[{"rateLimitId":"` + rlID + `","models":["ladder-prov/ladder-model"]}]}`, http.StatusCreated},
	} {
		code, raw := ls.adminDo(http.MethodPost, "/api/policies", `{"metadata":{"name":"ladder-tier-`+tc.name+`","owner":`+owner+`},"spec":`+tc.spec+`}`)
		if code != tc.want {
			t.Errorf("host-owned policy linking %s = %d %s, want %d", tc.name, code, raw, tc.want)
		}
	}
}

// A policy links only what its owner scope may use: its own scope's rows, its ancestors' and the system's, never a sibling project's.
func TestIntegration_ProjectPolicyLinksOnlyWhatItsScopeMayUse(t *testing.T) {
	t.Parallel()
	ls := newLadderStack(t)
	teamID := ls.mkTeam(t, "ladder-scope-team")
	own, sibling := ls.mkProject(t, "ladder-scope-own", teamID), ls.mkProject(t, "ladder-scope-sibling", teamID)
	pool := func(name, owner string) string {
		if owner != "" {
			owner = `,"owner":` + owner
		}
		return ls.create(t, "pools", `{"metadata":{"name":"`+name+`"`+owner+`},"spec":{"hostId":"`+ls.hostID+`"}}`)
	}
	siblingPool := pool("ladder-sibling-pool", `{"kind":"project","id":"`+sibling+`"}`)
	teamPool := pool("ladder-team-pool", `{"kind":"team","id":"`+teamID+`"}`)
	systemPool := pool("ladder-system-pool", "") // an admin create without an owner is system-owned
	siblingBudget := ls.create(t, "budgets", `{"metadata":{"name":"ladder-sibling-budget","owner":{"kind":"project","id":"`+sibling+`"}},"spec":{"amountUsd":"5"}}`)
	owner := `{"kind":"project","id":"` + own + `"}`
	for i, tc := range []struct {
		name, spec string
		ok         bool
	}{
		{"a sibling project's pool", `{"poolIds":["` + siblingPool + `"]}`, false},
		{"a sibling project's budget", `{"budgets":[{"budgetId":"` + siblingBudget + `"}]}`, false},
		{"its team's pool", `{"poolIds":["` + teamPool + `"]}`, true},
		{"a system pool", `{"poolIds":["` + systemPool + `"]}`, true},
	} {
		code, raw := ls.adminDo(http.MethodPost, "/api/policies", `{"metadata":{"name":"ladder-scope-policy-`+strconv.Itoa(i)+`","owner":`+owner+`},"spec":`+tc.spec+`}`)
		if ok := code == http.StatusCreated; ok != tc.ok || (!ok && code >= 500) {
			t.Errorf("project policy linking %s = %d %s, want accepted=%v", tc.name, code, raw, tc.ok)
		}
	}
}

// v1alpha3 manifests are the only form of the reshaped kinds: a v1alpha2 Policy is refused with a pointer to the new shape.
func TestIntegration_PreviousAPIVersionPolicyIsRefusedWithAPointer(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	code, _, raw := st.applyBundle("apiVersion: relay.wyolet.dev/v1alpha2\nkind: Policy\nmetadata:\n  name: legacy\nspec:\n  hostKeys: [old-key]\n", "dryRun=true")
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "v1alpha3") {
		t.Fatalf("v1alpha2 policy apply = %d %s, want 400 pointing at v1alpha3", code, raw)
	}
}
