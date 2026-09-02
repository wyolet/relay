//go:build integration

// iam_test.go covers the tenancy + subject control-plane surface end to
// end: the CRUD routes, the owner mirrors derived from spec, the PG
// cascades, the key principal/rotation surface, the seed loader's
// ordering, and the 0026 backfill of pre-existing relay keys.
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
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/app/user"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
	"github.com/wyolet/relay/pkg/ids"
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

// ── ServiceAccount / Group / Key CRUD ────────────────────────────────────

type keyRow struct {
	Metadata struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"owner"`
	} `json:"metadata"`
	Spec struct {
		Principal struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"principal"`
		PreviousKeyHash string     `json:"previousKeyHash"`
		GraceUntil      *time.Time `json:"graceUntil"`
	} `json:"spec"`
}

type createKeyBody struct {
	Plaintext string `json:"plaintext"`
	Key       keyRow `json:"key"`
}

// seedProjectAndAccount creates a team → project → service account through
// the control API and returns the project and account ids.
func (s *stack) seedProjectAndAccount(name string) (projectID, saID string) {
	s.t.Helper()
	code, raw := s.adminDo(http.MethodPost, "/api/teams",
		`{"metadata":{"name":"`+name+`-team"},"spec":{}}`)
	if code != http.StatusCreated {
		s.t.Fatalf("POST /api/teams = %d: %s", code, raw)
	}
	var team tenancyRow
	if err := json.Unmarshal(raw, &team); err != nil {
		s.t.Fatalf("decode team: %v", err)
	}
	code, raw = s.adminDo(http.MethodPost, "/api/projects",
		`{"metadata":{"name":"`+name+`"},"spec":{"teamId":"`+team.Metadata.ID+`"}}`)
	if code != http.StatusCreated {
		s.t.Fatalf("POST /api/projects = %d: %s", code, raw)
	}
	var proj tenancyRow
	if err := json.Unmarshal(raw, &proj); err != nil {
		s.t.Fatalf("decode project: %v", err)
	}
	code, raw = s.adminDo(http.MethodPost, "/api/service-accounts",
		`{"metadata":{"name":"`+name+`-sa"},"spec":{"projectId":"`+proj.Metadata.ID+`"}}`)
	if code != http.StatusCreated {
		s.t.Fatalf("POST /api/service-accounts = %d: %s", code, raw)
	}
	var sa tenancyRow
	if err := json.Unmarshal(raw, &sa); err != nil {
		s.t.Fatalf("decode service account: %v", err)
	}
	if sa.Metadata.Owner.Kind != "project" || sa.Metadata.Owner.ID != proj.Metadata.ID {
		s.t.Errorf("service account owner = %+v, want {project %s}", sa.Metadata.Owner, proj.Metadata.ID)
	}
	return proj.Metadata.ID, sa.Metadata.ID
}

func TestIntegration_KeyCreateAndRotate(t *testing.T) {
	st := newStack(t)
	projectID, saID := st.seedProjectAndAccount("ml-search")

	code, raw := st.adminDo(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"indexer-prod"},"spec":{"principal":{"kind":"serviceaccount","id":"`+saID+`"},"expiresAt":"2030-01-01T00:00:00Z"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/keys = %d: %s", code, raw)
	}
	var created createKeyBody
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if created.Plaintext == "" {
		t.Error("plaintext must be returned once on create")
	}
	if created.Key.Metadata.Owner.Kind != "project" || created.Key.Metadata.Owner.ID != projectID {
		t.Errorf("key owner = %+v, want the account's project", created.Key.Metadata.Owner)
	}
	if created.Key.Spec.Principal.Kind != "serviceaccount" || created.Key.Spec.Principal.ID != saID {
		t.Errorf("key principal = %+v", created.Key.Spec.Principal)
	}

	// A service account that does not exist reads as absent, not as a 400.
	code, raw = st.adminDo(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"ghost"},"spec":{"principal":{"kind":"serviceaccount","id":"0195f8a0-0000-7000-8000-0000000000ff"}}}`)
	if code != http.StatusNotFound {
		t.Fatalf("POST /api/keys with an unknown account = %d: %s", code, raw)
	}

	// The break-glass admin token may issue on any user's behalf, but the
	// user has to exist — the principal column is a real FK.
	code, raw = st.adminDo(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"ghost-user"},"spec":{"principal":{"kind":"user","id":"0195f8a0-0000-7000-8000-0000000000fe"}}}`)
	if code != http.StatusNotFound {
		t.Fatalf("POST /api/keys for an unknown user = %d: %s", code, raw)
	}

	// A user principal the caller cannot claim (no owner id to stamp, and
	// none supplied) fails validation rather than landing an orphan row.
	code, raw = st.adminDo(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"anonymous"},"spec":{"principal":{"kind":"user"}}}`)
	if code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /api/keys with no user id = %d: %s", code, raw)
	}

	// Rotation with a grace window keeps the previous hash alongside the new one.
	before := time.Now()
	code, raw = st.adminDo(http.MethodPost, "/api/keys/by-id/"+created.Key.Metadata.ID+"/rotate", `{"graceSeconds":3600}`)
	if code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", code, raw)
	}
	var rotated createKeyBody
	if err := json.Unmarshal(raw, &rotated); err != nil {
		t.Fatalf("decode rotate: %v", err)
	}
	if rotated.Plaintext == "" || rotated.Plaintext == created.Plaintext {
		t.Error("rotate must mint a fresh plaintext")
	}
	if rotated.Key.Spec.PreviousKeyHash == "" {
		t.Error("previousKeyHash should be set after a grace rotation")
	}
	if rotated.Key.Spec.GraceUntil == nil {
		t.Fatal("graceUntil should be set after a grace rotation")
	}
	if d := rotated.Key.Spec.GraceUntil.Sub(before); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("graceUntil is %v from now, want ~1h", d)
	}

	// Both plaintexts authenticate while the window is open.
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, plaintext := range []string{created.Plaintext, rotated.Plaintext} {
		if k, _ := st.cat.Current().KeyByHash(sha256Hex(plaintext)); k == nil {
			t.Errorf("plaintext %q should resolve during the grace window", plaintext[:12])
		}
	}

	// Above the configured maximum is a 400.
	code, raw = st.adminDo(http.MethodPost, "/api/keys/by-id/"+created.Key.Metadata.ID+"/rotate", `{"graceSeconds":999999}`)
	if code != http.StatusBadRequest {
		t.Fatalf("rotate above the cap = %d: %s", code, raw)
	}
}

func TestIntegration_KeyFilters(t *testing.T) {
	st := newStack(t)
	_, saID := st.seedProjectAndAccount("filters")

	code, raw := st.adminDo(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"live"},"spec":{"principal":{"kind":"serviceaccount","id":"`+saID+`"}}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/keys = %d: %s", code, raw)
	}
	code, raw = st.adminDo(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"stale"},"spec":{"principal":{"kind":"serviceaccount","id":"`+saID+`"},"expiresAt":"2020-01-01T00:00:00Z"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/keys = %d: %s", code, raw)
	}

	var list struct {
		Items []keyRow `json:"items"`
		Total int      `json:"total"`
	}
	code, raw = st.adminDo(http.MethodGet, "/api/keys?principal_kind=serviceaccount&principal_id="+saID, "")
	if code != http.StatusOK {
		t.Fatalf("list by principal = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 2 {
		t.Errorf("principal filter total = %d, want 2", list.Total)
	}

	code, raw = st.adminDo(http.MethodGet, "/api/keys?expired=true", "")
	if code != http.StatusOK {
		t.Fatalf("list expired = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || list.Items[0].Metadata.Name != "stale" {
		t.Errorf("expired=true => %d rows, want [stale]", list.Total)
	}
}

func TestIntegration_Groups(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()

	alice := &user.User{ID: ids.New(), Username: "alice"}
	if err := st.users.Upsert(ctx, alice); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	code, raw := st.adminDo(http.MethodPost, "/api/groups",
		`{"metadata":{"name":"data-science"},"spec":{"memberIds":["`+alice.ID+`"]}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/groups = %d: %s", code, raw)
	}
	var g tenancyRow
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("decode group: %v", err)
	}

	// A member id that is not a user is rejected before the row lands.
	code, raw = st.adminDo(http.MethodPost, "/api/groups",
		`{"metadata":{"name":"ghosts"},"spec":{"memberIds":["0195f8a0-0000-7000-8000-0000000000fd"]}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST /api/groups with a non-user member = %d: %s", code, raw)
	}

	// Reserved names belong to the built-in virtual groups.
	code, raw = st.adminDo(http.MethodPost, "/api/groups",
		`{"metadata":{"name":"system:authenticated"},"spec":{}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST /api/groups with a system: name = %d: %s", code, raw)
	}

	// Deleting the user drops the membership via the junction FK.
	if err := st.users.Delete(ctx, alice.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	stored, err := st.stores.Group.Get(ctx, g.Metadata.ID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if len(stored.Spec.MemberIDs) != 0 {
		t.Errorf("membership survived the user delete: %v", stored.Spec.MemberIDs)
	}
}

// ── seed ordering ────────────────────────────────────────────────────────

const seedSubjectsYAML = `apiVersion: relay.wyolet.dev/v1alpha2
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
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Group
metadata:
  name: data-science
  owner: {kind: system}
spec:
  members: [alice]
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: ServiceAccount
metadata:
  name: search-indexer
spec:
  project: ml-search
  policy: ml-search-pol
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Key
metadata:
  name: search-indexer-prod
  owner: {kind: project, name: ml-search}
spec:
  principal: {kind: serviceaccount, name: search-indexer}
  policy: ml-search-pol
  keyHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
`

func TestIntegration_SeedSubjects(t *testing.T) {
	st := newStack(t)
	pool := testPool(t)
	ctx := context.Background()

	alice := &user.User{ID: ids.New(), Username: "alice"}
	if err := st.users.Upsert(ctx, alice); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "subjects.yaml"), []byte(seedSubjectsYAML), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	res, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if res.Groups != 1 || res.ServiceAccounts != 1 || res.Keys != 1 {
		t.Fatalf("seeded %+v, want 1 group / 1 service account / 1 key", res)
	}

	groups, err := st.stores.Group.List(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("list groups: %v %d", err, len(groups))
	}
	if len(groups[0].Spec.MemberIDs) != 1 || groups[0].Spec.MemberIDs[0] != alice.ID {
		t.Errorf("group members = %v, want [%s]", groups[0].Spec.MemberIDs, alice.ID)
	}

	sas, err := st.stores.ServiceAccount.List(ctx)
	if err != nil || len(sas) != 1 {
		t.Fatalf("list service accounts: %v %d", err, len(sas))
	}
	keys, err := st.stores.Key.List(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list keys: %v %d", err, len(keys))
	}
	if keys[0].Spec.Principal.ID != sas[0].Meta.ID {
		t.Errorf("key principal = %q, want the seeded account %q", keys[0].Spec.Principal.ID, sas[0].Meta.ID)
	}
}

// ── migration 0026 backfill ──────────────────────────────────────────────

func migrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, os.Getenv("RELAY_TEST_PG_DSN"))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

// TestIntegration_KeyPrincipalBackfill drives migration 0026 against rows
// that predate it: one key owned by a real user, one whose owner carries
// no id.
func TestIntegration_KeyPrincipalBackfill(t *testing.T) {
	st := newStack(t) // truncates, and leaves the schema at head
	ctx := context.Background()
	userID := ids.New()
	if err := st.users.Upsert(ctx, &user.User{ID: userID, Username: "alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	m := migrator(t)
	if err := m.Migrate(25); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 25: %v", err)
	}

	p := testPool(t)
	insert := func(name, ownerJSON string) {
		t.Helper()
		_, err := p.Exec(ctx,
			`INSERT INTO relay_keys (id, name, display_name, key_hash, metadata, spec)
			 VALUES ($1, $2, '', $3, $4::jsonb, '{}'::jsonb)`,
			ids.New(), name, sha256Hex(name), ownerJSON)
		if err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	insert("owned", `{"owner":{"kind":"user","id":"`+userID+`"}}`)
	insert("orphan", `{"owner":{"kind":"user"}}`)

	if err := m.Migrate(26); err != nil {
		t.Fatalf("migrate up to 26: %v", err)
	}

	type row struct {
		saID, userID string
		principal    string
		ownerKind    string
	}
	read := func(name string) row {
		t.Helper()
		var r row
		var sa, u *string
		err := p.QueryRow(ctx,
			`SELECT principal_sa_id, principal_user_id, spec->'principal'->>'kind', metadata->'owner'->>'kind'
			   FROM relay_keys WHERE name = $1`, name).Scan(&sa, &u, &r.principal, &r.ownerKind)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if sa != nil {
			r.saID = *sa
		}
		if u != nil {
			r.userID = *u
		}
		return r
	}

	owned := read("owned")
	if owned.userID != userID || owned.saID != "" {
		t.Errorf("owned key principal columns = %+v, want the user only", owned)
	}
	if owned.principal != "user" {
		t.Errorf("owned key spec principal kind = %q, want user", owned.principal)
	}

	orphan := read("orphan")
	if orphan.saID == "" || orphan.userID != "" {
		t.Errorf("orphan key principal columns = %+v, want a service account only", orphan)
	}
	if orphan.principal != "serviceaccount" || orphan.ownerKind != "project" {
		t.Errorf("orphan key rewritten to %+v, want a project-owned serviceaccount principal", orphan)
	}

	var saName, projectName, teamName string
	if err := p.QueryRow(ctx,
		`SELECT sa.name, pr.name, tm.name
		   FROM service_accounts sa
		   JOIN projects pr ON pr.id = sa.project_id
		   JOIN teams tm ON tm.id = pr.team_id
		  WHERE sa.id = $1`, orphan.saID).Scan(&saName, &projectName, &teamName); err != nil {
		t.Fatalf("read generated account: %v", err)
	}
	if saName != "legacy-orphan" || projectName != "legacy" || teamName != "system" {
		t.Errorf("generated tenancy = %s / %s / %s, want legacy-orphan / legacy / system", saName, projectName, teamName)
	}

	// The CHECK keeps exactly one principal per row.
	if _, err := p.Exec(ctx,
		`INSERT INTO relay_keys (id, name, display_name, key_hash, metadata, spec)
		 VALUES ($1, 'no-principal', '', $2, '{}'::jsonb, '{}'::jsonb)`,
		ids.New(), sha256Hex("no-principal")); err == nil {
		t.Error("a key with neither principal column should violate the CHECK")
	}

	// Re-running converges: the fixed team/project ids are reused and each
	// key keeps exactly one generated account.
	if err := m.Migrate(25); err != nil {
		t.Fatalf("migrate down to 25 again: %v", err)
	}
	if err := m.Migrate(26); err != nil {
		t.Fatalf("migrate up to 26 again: %v", err)
	}
	var projects, accounts int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM projects WHERE name = 'legacy'`).Scan(&projects); err != nil {
		t.Fatalf("count legacy projects: %v", err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM service_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if projects != 1 || accounts != 1 {
		t.Errorf("after a second run: %d legacy projects / %d accounts, want 1 / 1", projects, accounts)
	}

	// Leave the schema at head: stepping to 26 dropped everything above it.
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate back up to head: %v", err)
	}
}
