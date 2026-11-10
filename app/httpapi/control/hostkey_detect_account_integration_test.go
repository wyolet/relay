//go:build integration

package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/meta"
)

// detectFixture is the policy-delete fixture whose host is an httptest upstream naming an organization per key value, mounted with an adapter registry that probes it.
type detectFixture struct {
	policyDeleteFixture
	probes  *atomic.Int64
	handler http.Handler
}

// orgsByKey is the upstream's answer per bearer value: an organization, or a 401 for any value it does not know.
var orgsByKey = map[string]string{"sk-a": "org-a", "sk-b": "org-b", "sk-c": "org-c", "sk-d": "org-d"}

func newDetectFixture(t *testing.T) (detectFixture, context.Context) {
	t.Helper()
	f, ctx := newPolicyDeleteFixture(t)
	probes := new(atomic.Int64)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		probes.Add(1)
		org, ok := orgsByKey[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Org-Id", org)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(up.Close)

	f.host.Spec.BaseURL = up.URL
	must(t, "upsert host", f.stores.Host.Upsert(ctx, f.host))
	prov := f.provider(t, ctx, "detect-provider")
	m := f.model(t, ctx, "detect-model", prov.Meta.ID)
	f.hostBinding(t, ctx, "detect-binding", m.Meta.ID, "")

	deps := mountDeps(t)
	deps.Stores = f.stores
	deps.Users = f.users
	deps.Catalog = f.cat
	deps.Authz = audit.Authorizer{Inner: testRBAC()}
	deps.Adapters = adapter.NewRegistry((&adapter.Spec{
		Name: adapters.OpenAI,
		Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Org:  &adapter.OrgProbe{Path: "/v1/models", OrgHeader: "X-Org-Id"},
	}).Build())
	r := chi.NewRouter()
	Mount(r, deps)
	return detectFixture{policyDeleteFixture: f, probes: probes, handler: r}, ctx
}

// key stores an env-ref host key holding value at host, owned by owner, in owner's one account there (made with the first such key).
func (f detectFixture) key(t *testing.T, ctx context.Context, name, value, hostID string, owner meta.Owner) *hostkey.HostKey {
	t.Helper()
	env := "DETECT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	t.Setenv(env, value)
	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner},
		Spec: hostkey.Spec{HostID: hostID, AccountID: f.ownersAccount(t, ctx, name, hostID, owner), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: env}},
	}
	must(t, "upsert host key "+name, f.stores.HostKey.Upsert(ctx, k))
	return k
}

// ownersAccount is owner's provider account at hostID, made under name's org name when owner has none there yet.
func (f detectFixture) ownersAccount(t *testing.T, ctx context.Context, name, hostID string, owner meta.Owner) string {
	t.Helper()
	all, err := f.stores.Account.List(ctx)
	must(t, "list accounts", err)
	for _, a := range all {
		if a.IsProvider() && !a.IsShared() && a.Spec.HostID == hostID && a.Meta.Owner == owner {
			return a.Meta.ID
		}
	}
	a := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: name + "-org", Owner: owner}, Spec: account.Spec{HostID: hostID}}
	must(t, "upsert account", f.stores.Account.Upsert(ctx, a))
	return a.Meta.ID
}

func (f detectFixture) detect(t *testing.T, ctx context.Context, keyID string) (*httptest.ResponseRecorder, detectedAccount) {
	t.Helper()
	must(t, "reload", f.cat.Reload(ctx))
	req := httptest.NewRequest(http.MethodPost, "/host-keys/by-id/"+keyID+"/detect-account", nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	var out detectedAccount
	if w.Code == http.StatusOK {
		must(t, "decode", json.Unmarshal(w.Body.Bytes(), &out))
	}
	return w, out
}

func (f detectFixture) accountOrg(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	a, err := f.stores.Account.Get(ctx, id)
	must(t, "get account", err)
	return a.Spec.OrgID
}

// A key alone in its fresh account names the account's org; a second key of another org is reported without touching the account, and an account holding that org is named as the match. No key moves.
func TestDetectAccount(t *testing.T) {
	f, ctx := newDetectFixture(t)
	system := meta.Owner{Kind: meta.OwnerSystem}
	first := f.key(t, ctx, "detect-first", "sk-a", f.host.Meta.ID, system)

	w, got := f.detect(t, ctx, first.Meta.ID)
	wantStatus(t, w, http.StatusOK)
	if got.OrgID != "org-a" || got.CurrentAccountID != first.Spec.AccountID || got.MatchingAccountID != first.Spec.AccountID {
		t.Fatalf("detect first = %+v, want org-a matching its own account %s", got, first.Spec.AccountID)
	}
	if org := f.accountOrg(t, ctx, first.Spec.AccountID); org != "org-a" {
		t.Fatalf("account orgId = %q, want filled with org-a", org)
	}

	second := f.key(t, ctx, "detect-second", "sk-b", f.host.Meta.ID, system)
	if second.Spec.AccountID != first.Spec.AccountID {
		t.Fatalf("second key account = %s, want the first key's %s", second.Spec.AccountID, first.Spec.AccountID)
	}
	w, got = f.detect(t, ctx, second.Meta.ID)
	wantStatus(t, w, http.StatusOK)
	if got.OrgID != "org-b" || got.CurrentAccountID != first.Spec.AccountID || got.MatchingAccountID != "" {
		t.Fatalf("detect second = %+v, want org-b with no matching account", got)
	}
	if org := f.accountOrg(t, ctx, first.Spec.AccountID); org != "org-a" {
		t.Fatalf("account orgId = %q after a disagreeing key, want org-a kept", org)
	}

	orgB := &account.Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "detect-org-b", Owner: system},
		Spec: account.Spec{HostID: f.host.Meta.ID, OrgID: "org-b"},
	}
	must(t, "upsert account", f.stores.Account.Upsert(ctx, orgB))
	w, got = f.detect(t, ctx, second.Meta.ID)
	wantStatus(t, w, http.StatusOK)
	if got.MatchingAccountID != orgB.Meta.ID || got.CurrentAccountID != first.Spec.AccountID {
		t.Fatalf("detect second = %+v, want matching %s", got, orgB.Meta.ID)
	}
	stored, err := f.stores.HostKey.Get(ctx, second.Meta.ID)
	must(t, "get key", err)
	if stored.Spec.AccountID != first.Spec.AccountID {
		t.Fatalf("second key moved to %s, want it left in %s", stored.Spec.AccountID, first.Spec.AccountID)
	}
}

// An account whose keys name different orgs is not filled; one whose keys agree is.
func TestDetectAccountFillsOnlyWhenAllKeysAgree(t *testing.T) {
	f, ctx := newDetectFixture(t)
	proj := meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID}
	c := f.key(t, ctx, "detect-c", "sk-c", f.host.Meta.ID, proj)
	f.key(t, ctx, "detect-d", "sk-d", f.host.Meta.ID, proj)

	w, got := f.detect(t, ctx, c.Meta.ID)
	wantStatus(t, w, http.StatusOK)
	if got.OrgID != "org-c" || got.MatchingAccountID != "" {
		t.Fatalf("detect = %+v, want org-c with no match", got)
	}
	if org := f.accountOrg(t, ctx, c.Spec.AccountID); org != "" {
		t.Fatalf("account orgId = %q, want empty while its keys disagree", org)
	}

	team := meta.Owner{Kind: meta.OwnerTeam, ID: f.proj.Spec.TeamID}
	one := f.key(t, ctx, "detect-c1", "sk-c", f.host.Meta.ID, team)
	f.key(t, ctx, "detect-c2", "sk-c", f.host.Meta.ID, team)
	w, got = f.detect(t, ctx, one.Meta.ID)
	wantStatus(t, w, http.StatusOK)
	if got.MatchingAccountID != one.Spec.AccountID {
		t.Fatalf("detect = %+v, want its own account filled and matched", got)
	}
	if org := f.accountOrg(t, ctx, one.Spec.AccountID); org != "org-c" {
		t.Fatalf("account orgId = %q, want org-c", org)
	}
}

func TestDetectAccountFailures(t *testing.T) {
	f, ctx := newDetectFixture(t)
	system := meta.Owner{Kind: meta.OwnerSystem}

	revoked := f.key(t, ctx, "detect-revoked", "sk-revoked", f.host.Meta.ID, system)
	w, _ := f.detect(t, ctx, revoked.Meta.ID)
	wantStatus(t, w, http.StatusBadGateway)
	if !strings.Contains(w.Body.String(), "401") {
		t.Fatalf("502 body = %s, want the upstream status", w.Body)
	}
	if org := f.accountOrg(t, ctx, revoked.Spec.AccountID); org != "" {
		t.Fatalf("account orgId = %q after a failed probe, want empty", org)
	}

	bare := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "detect-bare", Owner: system}, Spec: host.Spec{BaseURL: f.host.Spec.BaseURL}}
	must(t, "upsert host", f.stores.Host.Upsert(ctx, bare))
	unbound := f.key(t, ctx, "detect-unbound", "sk-a", bare.Meta.ID, system)
	before := f.probes.Load()
	w, _ = f.detect(t, ctx, unbound.Meta.ID)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	if f.probes.Load() != before {
		t.Fatal("a host without a probing shape was called")
	}

	w, _ = f.detect(t, ctx, meta.NewID())
	wantStatus(t, w, http.StatusNotFound)
}
