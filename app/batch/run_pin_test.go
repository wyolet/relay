package batch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/adapters"
)

// An item runs under the plans it started with: plans published while it runs (the submitter disabled, its fallback credential gone) do not reach it, so its failover still goes to the credential its route held.
func TestRunKeepsThePlanItStartedWith(t *testing.T) {
	var (
		mu     sync.Mutex
		called []string
		swap   func()
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		called = append(called, r.Header.Get("Authorization"))
		first := len(called) == 1
		mu.Unlock()
		if first {
			swap()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	var w plantest.World
	prov := w.Provider("vendor")
	h := w.Host("vendor", up.URL)
	m := w.Model(prov, "test-model")
	w.Bind(m, h, adapters.OpenAI)
	w.Credential(h, nil, plantest.System, "sk-first")
	fallback := w.Credential(h, nil, plantest.System, "sk-fallback")
	u := w.User()
	k := w.UserKey(u, "rk-secret")
	plans := w.Load(t)
	rn, _ := runnerOver(t, plans)

	swap = func() {
		gone := *fallback
		off := false
		gone.Spec.Enabled = &off
		plantest.Apply(t, plans, &gone)
		plantest.Apply(t, plans, &user.User{ID: u.ID, Username: u.Username, TokenVersion: u.TokenVersion, Disabled: true})
	}
	attr := Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: u.ID, CredentialKind: inference.CredentialKey, CredentialID: k.Meta.ID}
	status, _, err := rn.Run(context.Background(), "item-1", k.Spec.KeyHash, TokenClaims{}, attr, adapters.OpenAI, []byte(`{"model":"test-model"}`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("Run: status %d, err %v; want the fallback's 200 under the plan the item started with", status, err)
	}
	if len(called) != 2 || called[1] != "Bearer sk-fallback" {
		t.Fatalf("upstream calls = %v, want the first credential then the fallback", called)
	}
	if plans.Caller(t, k).Refused == nil {
		t.Fatal("the swapped plans did not refuse the disabled submitter; the test proves nothing")
	}
}
