//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// POST /keys keeps metadata.labels, and the key list filters on them.
func TestKeys_CreateKeepsLabels(t *testing.T) {
	w := newSharesWorld(t)
	rec := w.send("bob", http.MethodPost, "/keys", `{"metadata":{"displayName":"ci","labels":{"sa-type":"agent"}},"spec":{"principal":{"kind":"serviceaccount","id":"`+w.sa.Meta.ID+`"}}}`)
	wantStatus(t, rec, http.StatusCreated)
	var created createKeyResponse
	must(t, "decode", json.Unmarshal(rec.Body.Bytes(), &created.Body))
	stored, err := w.stores.Key.Get(t.Context(), created.Body.Key.Meta.ID)
	must(t, "key", err)
	if diff := cmp.Diff(map[string]string{"sa-type": "agent"}, stored.Meta.Labels); diff != "" {
		t.Errorf("labels (-want +got):\n%s", diff)
	}

	rec = w.send("bob", http.MethodGet, "/keys?label=sa-type=agent", "")
	wantStatus(t, rec, http.StatusOK)
	var list struct {
		Items []struct {
			Metadata struct{ ID string } `json:"metadata"`
		} `json:"items"`
	}
	must(t, "decode list", json.Unmarshal(rec.Body.Bytes(), &list))
	if len(list.Items) != 1 || list.Items[0].Metadata.ID != stored.Meta.ID {
		t.Errorf("?label=sa-type=agent lists %+v, want the key", list.Items)
	}
}
