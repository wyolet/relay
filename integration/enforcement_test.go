//go:build integration

// enforcement_test.go runs the control plane under RELAY_AUTHZ=rbac with
// real password logins: two teams, three users, and the flows M7 promises —
// a team admin owning their team, a developer in the other team seeing
// nothing of it, a viewer who reads but cannot write, and an apply that
// stops at the team boundary.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
)

// pickAuthorizer maps the stack's authz mode onto a concrete authorizer.
func pickAuthorizer(mode string, cat *appcatalog.Catalog) authz.Authorizer {
	if mode == "rbac" {
		return authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	}
	return authz.AlwaysAllowAuthenticated{}
}

// session is a logged-in browser: a cookie jar plus the request helpers.
type userSession struct {
	t      *testing.T
	base   string
	client *http.Client
}

func (s *stack) login(t *testing.T, username, password string) *userSession {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	us := &userSession{t: t, base: s.control.URL, client: &http.Client{Jar: jar}}
	code, raw := us.do(http.MethodPost, "/api/auth/login",
		`{"username":"`+username+`","password":"`+password+`"}`)
	if code != http.StatusOK {
		t.Fatalf("login %s = %d: %s", username, code, raw)
	}
	return us
}

func (u *userSession) do(method, path, body string) (int, []byte) {
	return u.doAs(method, path, body, "application/json")
}

func (u *userSession) doAs(method, path, body, contentType string) (int, []byte) {
	u.t.Helper()
	req, err := http.NewRequest(method, u.base+path, bytes.NewReader([]byte(body)))
	if err != nil {
		u.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := u.client.Do(req)
	if err != nil {
		u.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// seedLogin creates a user row with a real bcrypt password.
func (s *stack) seedLogin(t *testing.T, username, password string) string {
	t.Helper()
	hash, err := user.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	u := &user.User{ID: ids.New(), Username: username, PasswordHash: hash}
	if err := s.users.Upsert(context.Background(), u); err != nil {
		t.Fatalf("seed user %q: %v", username, err)
	}
	return u.ID
}

// bindRole grants roleName to a user at the given scope.
func (s *stack) bindRole(t *testing.T, name, roleID, userID, scopeKind, scopeID string) {
	t.Helper()
	scope := `{"kind":"` + scopeKind + `"}`
	owner := `{"kind":"` + scopeKind + `"}`
	if scopeID != "" {
		scope = `{"kind":"` + scopeKind + `","id":"` + scopeID + `"}`
		owner = scope
	}
	body := `{"metadata":{"name":"` + name + `","owner":` + owner + `},` +
		`"spec":{"roleId":"` + roleID + `","scope":` + scope + `,` +
		`"subjects":[{"kind":"user","id":"` + userID + `"}]}}`
	if code, raw := s.adminDo(http.MethodPost, "/api/role-bindings", body); code != http.StatusCreated {
		t.Fatalf("POST /api/role-bindings %s = %d: %s", name, code, raw)
	}
}

func (s *stack) mkTeam(t *testing.T, name string) string {
	t.Helper()
	code, raw := s.adminDo(http.MethodPost, "/api/teams", `{"metadata":{"name":"`+name+`"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/teams %s = %d: %s", name, code, raw)
	}
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode team: %v", err)
	}
	return row.Metadata.ID
}

func (s *stack) mkProject(t *testing.T, name, teamID string) string {
	t.Helper()
	code, raw := s.adminDo(http.MethodPost, "/api/projects",
		`{"metadata":{"name":"`+name+`"},"spec":{"teamId":"`+teamID+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/projects %s = %d: %s", name, code, raw)
	}
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	return row.Metadata.ID
}

func (s *stack) mkServiceAccount(t *testing.T, name, projectID string) string {
	t.Helper()
	code, raw := s.adminDo(http.MethodPost, "/api/service-accounts",
		`{"metadata":{"name":"`+name+`"},"spec":{"projectId":"`+projectID+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/service-accounts %s = %d: %s", name, code, raw)
	}
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode service account: %v", err)
	}
	return row.Metadata.ID
}

func TestIntegration_RBACEnforcement(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Two teams, one project each.
	teamA := st.mkTeam(t, "alpha")
	teamB := st.mkTeam(t, "beta")
	projA := st.mkProject(t, "alpha-one", teamA)
	projB := st.mkProject(t, "beta-one", teamB)
	saA := st.mkServiceAccount(t, "alpha-sa", projA)

	adminID := st.seedLogin(t, "tadmin", "pw-tadmin")
	devID := st.seedLogin(t, "dev", "pw-dev")
	viewerID := st.seedLogin(t, "viewer", "pw-viewer")

	st.bindRole(t, "alpha-admins", roles["team-admin"].Meta.ID, adminID, "team", teamA)
	st.bindRole(t, "beta-devs", roles["developer"].Meta.ID, devID, "project", projB)
	st.bindRole(t, "alpha-viewers", roles["viewer"].Meta.ID, viewerID, "team", teamA)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	admin := st.login(t, "tadmin", "pw-tadmin")
	dev := st.login(t, "dev", "pw-dev")
	viewer := st.login(t, "viewer", "pw-viewer")

	t.Run("whoami carries subjects and scopes", func(t *testing.T) {
		code, raw := admin.do(http.MethodGet, "/api/auth/whoami", "")
		if code != http.StatusOK {
			t.Fatalf("whoami = %d: %s", code, raw)
		}
		var out struct {
			Subjects []string `json:"subjects"`
			Scopes   []string `json:"scopes"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Subjects) == 0 || out.Subjects[0] != "user:"+adminID {
			t.Fatalf("subjects = %v, want user:%s first", out.Subjects, adminID)
		}
		if len(out.Scopes) != 1 || out.Scopes[0] != "team:"+teamA {
			t.Fatalf("scopes = %v, want [team:%s]", out.Scopes, teamA)
		}
	})

	t.Run("team admin owns their team", func(t *testing.T) {
		code, raw := admin.do(http.MethodPost, "/api/projects",
			`{"metadata":{"name":"alpha-two"},"spec":{"teamId":"`+teamA+`"}}`)
		if code != http.StatusCreated {
			t.Fatalf("create project in own team = %d: %s", code, raw)
		}
		code, raw = admin.do(http.MethodPost, "/api/keys",
			`{"metadata":{"name":"alpha-key"},"spec":{"principal":{"kind":"serviceaccount","id":"`+saA+`"}}}`)
		if code != http.StatusCreated {
			t.Fatalf("create key in own team = %d: %s", code, raw)
		}
		code, raw = admin.do(http.MethodPost, "/api/projects",
			`{"metadata":{"name":"beta-two"},"spec":{"teamId":"`+teamB+`"}}`)
		if code == http.StatusCreated {
			t.Fatalf("create project in the other team succeeded: %s", raw)
		}
	})

	t.Run("developer in another team sees nothing of this one", func(t *testing.T) {
		code, raw := dev.do(http.MethodGet, "/api/projects/alpha-one", "")
		if code != http.StatusNotFound {
			t.Fatalf("foreign project read = %d, want 404: %s", code, raw)
		}
		code, raw = dev.do(http.MethodPost, "/api/keys",
			`{"metadata":{"name":"sneaky"},"spec":{"principal":{"kind":"serviceaccount","id":"`+saA+`"}}}`)
		if code != http.StatusForbidden && code != http.StatusNotFound {
			t.Fatalf("foreign key create = %d, want 403/404: %s", code, raw)
		}
		// A kind the developer role does grant lists, and lists filtered.
		code, raw = dev.do(http.MethodGet, "/api/keys", "")
		if code != http.StatusOK {
			t.Fatalf("developer key list = %d: %s", code, raw)
		}
		var list struct {
			Items []keyRow `json:"items"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		for _, it := range list.Items {
			if it.Metadata.Owner.ID != projB {
				t.Fatalf("developer sees key %q outside their project", it.Metadata.Name)
			}
		}
	})

	t.Run("viewer reads but cannot mutate", func(t *testing.T) {
		code, raw := viewer.do(http.MethodGet, "/api/keys", "")
		if code != http.StatusOK {
			t.Fatalf("viewer key list = %d: %s", code, raw)
		}
		var list struct {
			Items []keyRow `json:"items"`
			Total int      `json:"total"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		if list.Total == 0 {
			t.Fatal("viewer sees no keys in their own team")
		}
		id := list.Items[0].Metadata.ID
		code, raw = viewer.do(http.MethodDelete, "/api/keys/by-id/"+id, "")
		if code != http.StatusForbidden {
			t.Fatalf("viewer delete = %d, want 403: %s", code, raw)
		}
	})

	t.Run("apply writes only inside the team", func(t *testing.T) {
		inside := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Project\n" +
			"metadata:\n  name: alpha-applied\n  owner: {kind: team, name: alpha}\n" +
			"spec:\n  team: alpha\n"
		code, raw := admin.doAs(http.MethodPost, "/api/apply", inside, "application/yaml")
		if code != http.StatusOK {
			t.Fatalf("apply inside the team = %d: %s", code, raw)
		}
		outside := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Project\n" +
			"metadata:\n  name: beta-applied\n  owner: {kind: team, name: beta}\n" +
			"spec:\n  team: beta\n"
		code, raw = admin.doAs(http.MethodPost, "/api/apply", outside, "application/yaml")
		if code != http.StatusForbidden {
			t.Fatalf("apply outside the team = %d, want 403: %s", code, raw)
		}
		if code, raw := st.adminDo(http.MethodGet, "/api/projects/beta-applied", ""); code != http.StatusNotFound {
			t.Fatalf("the denied apply wrote a row: %d %s", code, raw)
		}
	})

	t.Run("users list is gated and carries no credentials", func(t *testing.T) {
		if code, raw := dev.do(http.MethodGet, "/api/users", ""); code != http.StatusForbidden {
			t.Fatalf("developer users list = %d, want 403: %s", code, raw)
		}
		code, raw := st.adminDo(http.MethodGet, "/api/users", "")
		if code != http.StatusOK {
			t.Fatalf("admin users list = %d: %s", code, raw)
		}
		if bytes.Contains(raw, []byte("$2a$")) || bytes.Contains(raw, []byte("oidc")) {
			t.Fatalf("users list leaked a credential field: %s", raw)
		}
	})

	t.Run("a team admin owns the team row itself", func(t *testing.T) {
		if code, raw := admin.do(http.MethodGet, "/api/teams/alpha", ""); code != http.StatusOK {
			t.Fatalf("team-admin GET own team = %d, want 200: %s", code, raw)
		}
		if code, raw := admin.do(http.MethodGet, "/api/teams/beta", ""); code != http.StatusNotFound {
			t.Fatalf("team-admin GET sibling team = %d, want 404: %s", code, raw)
		}
		if code, raw := admin.do(http.MethodGet, "/api/projects/alpha-one", ""); code != http.StatusOK {
			t.Fatalf("team-admin GET own project = %d, want 200: %s", code, raw)
		}
		if code, raw := admin.do(http.MethodGet, "/api/projects/beta-one", ""); code != http.StatusNotFound {
			t.Fatalf("team-admin GET sibling project = %d, want 404: %s", code, raw)
		}
	})

	t.Run("a developer sees the project they work in", func(t *testing.T) {
		if code, raw := dev.do(http.MethodGet, "/api/projects/beta-one", ""); code != http.StatusOK {
			t.Fatalf("developer GET own project = %d, want 200: %s", code, raw)
		}
		code, raw := dev.do(http.MethodGet, "/api/projects", "")
		if code != http.StatusOK {
			t.Fatalf("developer project list = %d: %s", code, raw)
		}
		var list struct {
			Items []tenancyRow `json:"items"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 1 || list.Items[0].Metadata.ID != projB {
			t.Fatalf("developer project list = %+v, want only their own project", list.Items)
		}
	})
}

// A YAML-bootstrapped user logs in through the identity fallback; the session
// must still carry the seeded row's UUID, because every id-keyed surface
// downstream (key principals, owner ids, subjects) rejects a slug.
func TestIntegration_YAMLLoginCarriesTheRowUUID(t *testing.T) {
	dir := t.TempDir()
	yaml := "apiVersion: relay.wyolet.dev/v1\nkind: User\nmetadata:\n  name: bootstrap-admin\n" +
		"spec:\n  username: bootadmin\n  email: bootadmin@example.test\n" +
		"  password: 'boot-pw-12345'\n  roles:\n    - admin\n"
	if err := os.WriteFile(filepath.Join(dir, "admin.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write identity yaml: %v", err)
	}

	st := newStackAuthz(t, "", dir)

	// Force the identity fallback: the seed is if-absent, so a YAML password
	// rotated after the row was seeded leaves the row's hash stale and the
	// DB-first branch fails. That is the only way into the YAML path once a
	// user store exists, and the path this test is about.
	ctx := context.Background()
	row, err := st.users.ByUsername(ctx, "bootadmin")
	if err != nil || row == nil {
		t.Fatalf("seeded row lookup: %v", err)
	}
	stale, err := user.HashPassword("a-different-password")
	if err != nil {
		t.Fatal(err)
	}
	row.PasswordHash = stale
	if err := st.users.Upsert(ctx, row); err != nil {
		t.Fatalf("stale the row hash: %v", err)
	}

	us := st.login(t, "bootadmin", "boot-pw-12345")

	code, raw := us.do(http.MethodGet, "/api/auth/whoami", "")
	if code != http.StatusOK {
		t.Fatalf("whoami = %d: %s", code, raw)
	}
	var who struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &who); err != nil {
		t.Fatal(err)
	}
	if !ids.Valid(who.UserID) {
		t.Fatalf("whoami user_id = %q, want the seeded row UUID", who.UserID)
	}

	// The id the session carries is the one a personal key accepts.
	code, raw = us.do(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"bootstrap-key"},"spec":{"principal":{"kind":"user","id":"`+who.UserID+`"}}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/keys with the whoami id = %d, want 201: %s", code, raw)
	}
}
