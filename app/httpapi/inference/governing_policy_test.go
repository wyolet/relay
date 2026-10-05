package inference

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
)

// governed sends one request through AuthenticateMiddleware, which resolves no policy, and returns what GoverningPolicy and the principal say inside the handler.
func governed(t *testing.T, f principalFixture, k *key.Key, plaintext string) (status int, governing, onPrincipal *policy.Policy) {
	t.Helper()
	st := f.stack(t, k)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		governing = GoverningPolicy(r.Context())
		onPrincipal = PrincipalFrom(r.Context()).Policy
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodPost, "/otlp/v1/traces", nil)
	r.Header.Set("Authorization", "Bearer "+plaintext)
	w := httptest.NewRecorder()
	ClassifyMiddleware()(AuthenticateMiddleware(st.cat, f.tokens)(inner)).ServeHTTP(w, r)
	return w.Code, governing, onPrincipal
}

func TestGoverningPolicyFollowsTheInferenceOrder(t *testing.T) {
	t.Run("the key's own policy first", func(t *testing.T) {
		f := newPrincipalFixture()
		k := saKey(f, "sk-key-pol")
		k.Spec.PolicyID = f.keyPol.Meta.ID
		if status, got, _ := governed(t, f, k, "sk-key-pol"); status != http.StatusOK || got == nil || got.Meta.ID != f.keyPol.Meta.ID {
			t.Errorf("status %d policy %+v, want the key's policy", status, got)
		}
	})

	t.Run("then the service account's", func(t *testing.T) {
		f := newPrincipalFixture()
		f.bindings = []*policybinding.PolicyBinding{boundTo(f, "everyone", 1, f.boundPol.Meta.ID, "group:system:authenticated")}
		status, got, onPrincipal := governed(t, f, saKey(f, "sk-sa-pol"), "sk-sa-pol")
		if status != http.StatusOK || got == nil || got.Meta.ID != f.saPol.Meta.ID {
			t.Errorf("status %d policy %+v, want the service account's policy", status, got)
		}
		// Looking the policy up leaves the principal as authentication built it.
		if onPrincipal != nil {
			t.Errorf("principal carries policy %q after the lookup", onPrincipal.Meta.Name)
		}
	})

	t.Run("then the project's bindings", func(t *testing.T) {
		f := newPrincipalFixture()
		f.sa.Spec.PolicyID = ""
		f.bindings = []*policybinding.PolicyBinding{
			boundTo(f, "someone-else", 1, f.keyPol.Meta.ID, "user:"+f.user),
			boundTo(f, "the-account", 5, f.boundPol.Meta.ID, "serviceaccount:"+f.sa.Meta.ID),
		}
		if status, got, _ := governed(t, f, saKey(f, "sk-bound"), "sk-bound"); status != http.StatusOK || got == nil || got.Meta.ID != f.boundPol.Meta.ID {
			t.Errorf("status %d policy %+v, want the bound policy", status, got)
		}
	})
}

func TestGoverningPolicyRejectsNothing(t *testing.T) {
	// Inference answers 403 to both of these; a reporter is still admitted.
	t.Run("no policy", func(t *testing.T) {
		f := newPrincipalFixture()
		f.sa.Spec.PolicyID = ""
		if status, got, _ := governed(t, f, saKey(f, "sk-none"), "sk-none"); status != http.StatusOK || got != nil {
			t.Errorf("status %d policy %+v, want 200 and no policy", status, got)
		}
	})

	t.Run("disabled policy still governs", func(t *testing.T) {
		f := newPrincipalFixture()
		off := false
		f.saPol.Spec.Enabled = &off
		status, got, _ := governed(t, f, saKey(f, "sk-off"), "sk-off")
		if status != http.StatusOK || got == nil || got.Meta.ID != f.saPol.Meta.ID || got.IsEnabled() {
			t.Errorf("status %d policy %+v, want 200 and the disabled policy", status, got)
		}
	})
}
