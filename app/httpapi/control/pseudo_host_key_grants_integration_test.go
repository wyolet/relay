//go:build integration

package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/ids"
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
