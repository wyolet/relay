//go:build integration

// iam_test.go covers the Team/Project control-plane surface end to end:
// the CRUD routes, the owner mirror derived from spec.teamId, the PG
// cascade behind a team delete, and the seed loader's tenancy ordering.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/seed"
)

// testPool opens a second pool for the seed loader, which takes a pool
// rather than the already-wired stores.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("RELAY_TEST_PG_DSN"))
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// adminDo issues an admin-token request against the control plane and
// returns the status plus the raw body.
func (s *stack) adminDo(method, path, body string) (int, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.control.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		s.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

type tenancyRow struct {
	Metadata struct {
		ID          string            `json:"id"`
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
		Owner       struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"owner"`
	} `json:"metadata"`
	Spec struct {
		TeamID string `json:"teamId"`
	} `json:"spec"`
}

func TestIntegration_TeamProjectCRUD(t *testing.T) {
	st := newStack(t)

	code, raw := st.adminDo(http.MethodPost, "/api/teams",
		`{"metadata":{"name":"platform","displayName":"Platform"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/teams = %d: %s", code, raw)
	}
	var team tenancyRow
	if err := json.Unmarshal(raw, &team); err != nil {
		t.Fatalf("decode team: %v", err)
	}

	code, raw = st.adminDo(http.MethodPost, "/api/projects",
		`{"metadata":{"name":"ml-search","annotations":{"wyolet.com/cost-center":"1042"}},"spec":{"teamId":"`+team.Metadata.ID+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/projects = %d: %s", code, raw)
	}
	var proj tenancyRow
	if err := json.Unmarshal(raw, &proj); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if proj.Metadata.Owner.Kind != "team" || proj.Metadata.Owner.ID != team.Metadata.ID {
		t.Errorf("owner = %+v, want {team %s}", proj.Metadata.Owner, team.Metadata.ID)
	}

	// Annotations survive create → get.
	code, raw = st.adminDo(http.MethodGet, "/api/projects/ml-search", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/projects/ml-search = %d: %s", code, raw)
	}
	var fetched tenancyRow
	if err := json.Unmarshal(raw, &fetched); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if fetched.Metadata.Annotations["wyolet.com/cost-center"] != "1042" {
		t.Errorf("annotations = %v, want cost-center 1042", fetched.Metadata.Annotations)
	}

	// An unknown team is a 400, not a dangling project.
	code, raw = st.adminDo(http.MethodPost, "/api/projects",
		`{"metadata":{"name":"orphan"},"spec":{"teamId":"0195f8a0-0000-7000-8000-0000000000ff"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST /api/projects with unknown team = %d: %s", code, raw)
	}

	// Deleting the team cascades in PG.
	code, raw = st.adminDo(http.MethodDelete, "/api/teams/by-id/"+team.Metadata.ID, "")
	if code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("DELETE /api/teams = %d: %s", code, raw)
	}
	projects, err := st.stores.Project.List(context.Background())
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects survived the team delete: %d", len(projects))
	}
}

const seedTeamYAML = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata:
  name: platform
  owner: {kind: system}
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Project
metadata:
  name: ml-search
spec:
  team: platform
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata:
  name: ml-search-pol
  owner: {kind: project, name: ml-search}
spec: {}
`

func TestIntegration_SeedTenancy(t *testing.T) {
	st := newStack(t)
	pool := testPool(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tenancy.yaml"), []byte(seedTeamYAML), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	ctx := context.Background()

	res, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if res.Teams != 1 || res.Projects != 1 || res.Policies != 1 {
		t.Fatalf("seeded %+v, want 1 team / 1 project / 1 policy", res)
	}

	proj, err := st.stores.Project.List(ctx)
	if err != nil || len(proj) != 1 {
		t.Fatalf("list projects: %v %d", err, len(proj))
	}
	pols, err := st.stores.Policy.List(ctx)
	if err != nil || len(pols) != 1 {
		t.Fatalf("list policies: %v %d", err, len(pols))
	}
	if pols[0].Meta.Owner.ID != proj[0].Meta.ID {
		t.Errorf("policy owner id = %q, want project %q", pols[0].Meta.Owner.ID, proj[0].Meta.ID)
	}

	// A dirty project is left alone on re-seed.
	proj[0].Meta.Dirty = true
	proj[0].Meta.DisplayName = "Operator Edited"
	if err := st.stores.Project.Upsert(ctx, proj[0]); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	res, err = seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir})
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if res.Projects != 0 || res.Skipped == 0 {
		t.Fatalf("re-seed %+v, want the dirty project skipped", res)
	}
	after, err := st.stores.Project.Get(ctx, proj[0].Meta.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if after.Meta.DisplayName != "Operator Edited" {
		t.Errorf("dirty project was clobbered: %q", after.Meta.DisplayName)
	}
}
