//go:build integration

package control

import (
	"net/http"
	"strings"
	"testing"
)

// A caller who sees the service account but may not take every delete is refused before any list is answered, expect given or not, dry run or not, and the body names none of the rows it may not see: the service account's delete refused, or its key or its account's binding hidden.
func TestPseudoAgentRemoval_RefusedBeforeAnyList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(w *agentRemovalWorld, held string)
		status int
	}{
		{"the service account's delete refused, its key and binding hidden", func(w *agentRemovalWorld, held string) {
			*w.denied = "service-accounts.delete"
			w.hidden[w.agent.Key.ID], w.hidden[held] = true, true
		}, http.StatusForbidden},
		{"its key hidden", func(w *agentRemovalWorld, held string) { w.hidden[w.agent.Key.ID] = true }, http.StatusNotFound},
		{"its account's binding hidden", func(w *agentRemovalWorld, held string) { w.hidden[held] = true }, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newAgentRemovalWorld(t)
			held := w.holdingOf(t, w.agent.Account.ID)
			tc.setup(w, held.Meta.ID)
			defer func() { *w.denied = "" }()
			secrets := []string{w.agent.Key.ID, w.agent.Key.Name, held.Meta.ID, held.Meta.Name}
			for _, spec := range []map[string]any{{}, {"expect": []string{}}} {
				for _, dry := range []bool{true, false} {
					rec, _ := w.remove(t, "bob", spec, dry)
					body := rec.Body.String()
					if r := refusalOf(t, rec); rec.Code != tc.status || r.Pointer != "/spec/serviceAccountId" {
						t.Errorf("expect %v, dry run %v: %d %s, want %d at /spec/serviceAccountId", spec["expect"], dry, rec.Code, body, tc.status)
					}
					for _, s := range secrets {
						if strings.Contains(body, s) {
							t.Errorf("expect %v, dry run %v: the answer names %q: %s", spec["expect"], dry, s, body)
						}
					}
				}
			}
			if left := w.stillStored(t, w.agentRows(t)); len(left) != 6 {
				t.Errorf("rows left %v, want all six", left)
			}
		})
	}
}
