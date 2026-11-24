//go:build integration

package control

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// A name the server derives is minted against the rows stored and those the same request makes: a second identical request goes through with its names suffixed, as a second create of each row would.
func TestPseudoKinds_RepeatedRequestSuffixesNames(t *testing.T) {
	t.Run("limits", func(t *testing.T) {
		w := newPolicyStepWorld(t)
		w.loadPlans(t)
		tgt := target("project", w.projTwo.Meta.ID)
		for _, limit := range []map[string]any{aBudget, aRateLimit} {
			var names []string
			for range 2 {
				rec, out := w.limits(t, "bob", tgt, limit, false)
				wantStatus(t, rec, http.StatusCreated)
				made := out.Budget
				if made == nil {
					made = out.RateLimit
				}
				names = append(names, made.Name)
				w.loadPlans(t)
			}
			if diff := cmp.Diff([]string{"ci-cap", "ci-cap-2"}, names); diff != "" {
				t.Errorf("names (-want +got):\n%s", diff)
			}
		}
	})
	t.Run("agents", func(t *testing.T) {
		w := newPolicyStepWorld(t)
		w.loadPlans(t)
		body := w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}, "key": map[string]any{"displayName": "CI key"}})
		var first, second PseudoAgentCreated
		wantStatus(t, w.postPseudo(t, "bob", "/pseudo-agents", body, false, &first), http.StatusCreated)
		w.loadPlans(t)
		wantStatus(t, w.postPseudo(t, "bob", "/pseudo-agents", body, false, &second), http.StatusCreated)
		for i := range first.Created {
			if f, s := first.Created[i].Name, second.Created[i].Name; s == f || !strings.HasSuffix(s, "-2") {
				t.Errorf("%s: second name %q after %q, want it suffixed -2", first.Created[i].Kind, s, f)
			}
		}
	})
	t.Run("host-keys", func(t *testing.T) {
		w := newHostKeyFormWorld(t)
		body := w.body("alice", map[string]any{"value": keyBare, "account": map[string]any{"displayName": "Work"}})
		_, first := w.post(t, "alice", body, false)
		w.loadPlans(t)
		rec, second := w.post(t, "alice", w.body("alice", map[string]any{"value": keyBare, "account": map[string]any{"displayName": "Work"}}), false)
		wantStatus(t, rec, http.StatusCreated)
		if diff := cmp.Diff([]string{"account/work-2", "host-key/work-key-2"}, createdNames(second)); diff != "" {
			t.Errorf("second (-want +got):\n%s; first %v", diff, createdNames(first))
		}
	})
}
