//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// An operator edit made through the API, exported and applied back, must report no drift: the row is dirty but matches the bundle.
func TestIntegration_ExportAfterAnAPIEditReappliesWithoutDrift(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	if code, _, raw := st.applyBundle(bundle, ""); code != http.StatusOK {
		t.Fatalf("seed bundle: %d %s", code, raw)
	}
	sas, err := st.stores.ServiceAccount.List(ctx)
	if err != nil || len(sas) != 1 {
		t.Fatalf("list service accounts: %v (%d)", err, len(sas))
	}
	code, raw := st.adminDo(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"indexer-prod"},"spec":{"principal":{"kind":"serviceaccount","id":"`+sas[0].Meta.ID+`"}}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/keys = %d: %s", code, raw)
	}

	// edit reads a row through the API, changes its JSON and writes it back, which marks it dirty.
	edit := func(plural, name string, change func(row map[string]any)) {
		t.Helper()
		code, body := st.adminDo(http.MethodGet, "/api/"+plural+"/"+name, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s/%s = %d: %s", plural, name, code, body)
		}
		var row map[string]any
		if err := json.Unmarshal(body, &row); err != nil {
			t.Fatalf("decode %s/%s: %v", plural, name, err)
		}
		change(row)
		blob, _ := json.Marshal(row)
		id := row["metadata"].(map[string]any)["id"].(string)
		if code, body = st.adminDo(http.MethodPut, "/api/"+plural+"/by-id/"+id, string(blob)); code != http.StatusOK {
			t.Fatalf("PUT %s/%s = %d: %s", plural, name, code, body)
		}
	}
	edit("keys", "indexer-prod", func(r map[string]any) {
		r["spec"].(map[string]any)["revokedAt"] = "2026-01-01T00:00:00Z"
	})
	edit("teams", "platform", func(r map[string]any) {
		r["metadata"].(map[string]any)["displayName"] = "Platform Engineering"
	})
	keys, err := st.stores.Key.List(ctx)
	if err != nil || len(keys) != 1 || !keys[0].Meta.Dirty || keys[0].Spec.RevokedAt == nil {
		t.Fatalf("the API edit did not leave a revoked, dirty key: %v %+v", err, keys)
	}

	code, body := st.adminDo(http.MethodGet, "/api/export", "")
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, body)
	}
	for _, query := range []string{"dryRun=true", ""} {
		_, plan, raw := st.applyBundle(string(body), query)
		for _, e := range plan.Plan {
			if e.Action != "unchanged" {
				t.Fatalf("apply?%s reported %s on %s/%s (%v)\n%s", query, e.Action, e.Kind, e.Name, e.ChangedFields, raw)
			}
		}
		if plan.Counts.SkipDirty != 0 || plan.Counts.Unchanged != 5 {
			t.Fatalf("apply?%s counts = %+v, want five unchanged rows", query, plan.Counts)
		}
	}
}
