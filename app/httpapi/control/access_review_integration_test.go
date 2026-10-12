//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/ids"
)

// The whole control API over Postgres: for each request, the access review's answer for the caller matches what the request itself then gets.
func TestAccessReviewMatchesTheRealRequest(t *testing.T) {
	ctx := t.Context()
	pool := storagetest.Pool(t)
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if err := role.SeedBuiltins(ctx, stores.Role, nil, nil); err != nil {
		t.Fatalf("seed roles: %v", err)
	}
	roles, err := stores.Role.List(ctx)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	var developer string
	for _, r := range roles {
		if r.Meta.Name == "developer" {
			developer = r.Meta.ID
		}
	}

	users := user.NewStore(pool)
	aliceID, rootID := ids.New(), ids.New()
	must(t, "upsert alice", users.Upsert(ctx, &user.User{ID: aliceID, Username: "alice"}))
	must(t, "upsert root", users.Upsert(ctx, &user.User{ID: rootID, Username: "root", Roles: []string{user.RoleAdmin}}))

	yes := true
	tm := &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "review-team", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tm.Spec.Enabled = &yes
	must(t, "upsert team", stores.Team.Upsert(ctx, tm))
	mkProject := func(name string) *project.Project {
		p := &project.Project{Meta: meta.Metadata{ID: ids.New(), Name: name}, Spec: project.Spec{TeamID: tm.Meta.ID, Enabled: &yes}}
		p.StampOwner()
		must(t, "upsert project "+name, stores.Project.Upsert(ctx, p))
		return p
	}
	mine, theirs := mkProject("review-mine"), mkProject("review-theirs")
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "alice-dev"}}
	rb.Spec.RoleID, rb.Spec.Scope, rb.Spec.Enabled = developer, meta.Owner{Kind: meta.OwnerProject, ID: mine.Meta.ID}, &yes
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: aliceID}}
	rb.StampOwner()
	must(t, "upsert role binding", stores.RoleBinding.Upsert(ctx, rb))
	must(t, "reload", cat.Reload(ctx))

	deps := mountDeps(t)
	deps.Stores, deps.Catalog, deps.Users = stores, cat, users
	deps.Authz = audit.Authorizer{Inner: authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}, Snap: cat.Current}
	actors := map[string]*actor.Actor{
		"alice": {UserID: aliceID, Username: "alice", Subjects: appcatalog.UserSubjects(aliceID, nil, nil)},
		"root":  {UserID: rootID, Username: "root", Roles: []string{user.RoleAdmin}, Subjects: appcatalog.UserSubjects(rootID, nil, nil)},
	}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-User", actors))
	Mount(r, deps)
	send := func(who, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-User", who)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	// review asks as alice herself and as root on alice's behalf; a row alice cannot see answers her 404.
	review := func(action, resource string) (selfCode int, self, onBehalf accessReview) {
		w := send("alice", http.MethodPost, "/access-review", `{"action":"`+action+`","resource":`+resource+`}`)
		if w.Code == http.StatusOK {
			must(t, "decode self review", json.Unmarshal(w.Body.Bytes(), &self))
		}
		w2 := send("root", http.MethodPost, "/access-review", `{"subject":{"kind":"user","name":"alice"},"action":"`+action+`","resource":`+resource+`}`)
		if w2.Code != http.StatusOK {
			t.Fatalf("root reviewing alice %s on %s = %d: %s", action, resource, w2.Code, w2.Body)
		}
		must(t, "decode review", json.Unmarshal(w2.Body.Bytes(), &onBehalf))
		return w.Code, self, onBehalf
	}

	saBody := func(name, projectID string) string {
		return `{"metadata":{"name":"` + name + `"},"spec":{"projectId":"` + projectID + `","enabled":true}}`
	}
	tests := []struct {
		name             string
		action, resource string
		method, path     string
		body             string
		allowed          bool
	}{
		{"read my project", "projects.get", `{"kind":"projects","name":"review-mine"}`, http.MethodGet, "/projects/review-mine", "", true},
		{"read a project I am not in", "projects.get", `{"kind":"projects","name":"review-theirs"}`, http.MethodGet, "/projects/review-theirs", "", false},
		{"read my project's team", "teams.get", `{"kind":"teams","name":"review-team"}`, http.MethodGet, "/teams/review-team", "", true},
		{"delete my project", "projects.delete", `{"kind":"projects","name":"review-mine"}`, http.MethodDelete, "/projects/by-id/" + mine.Meta.ID, "", false},
		{"create an account in my project", "service-accounts.create", `{"kind":"service-accounts","owner":{"kind":"project","name":"review-mine"}}`,
			http.MethodPost, "/service-accounts", saBody("review-ci", mine.Meta.ID), true},
		{"create an account elsewhere", "service-accounts.create", `{"kind":"service-accounts","owner":{"kind":"project","id":"` + theirs.Meta.ID + `"}}`,
			http.MethodPost, "/service-accounts", saBody("review-ci-2", theirs.Meta.ID), false},
		{"list the users", "users.list", `{"kind":"users"}`, http.MethodGet, "/users", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selfCode, self, onBehalf := review(tt.action, tt.resource)
			real := send("alice", tt.method, tt.path, tt.body)
			allowed := real.Code >= 200 && real.Code < 300
			if allowed != tt.allowed {
				t.Fatalf("the request got %d, want allowed=%v: %s", real.Code, tt.allowed, real.Body)
			}
			if onBehalf.Allowed != allowed {
				t.Fatalf("review says allowed=%v (%s), the request got %d: %s", onBehalf.Allowed, onBehalf.Reason, real.Code, real.Body)
			}
			switch {
			case selfCode == http.StatusNotFound && real.Code != http.StatusNotFound:
				t.Fatalf("self review hid the row, the request got %d", real.Code)
			case selfCode == http.StatusOK && self.Allowed != allowed:
				t.Fatalf("self review says allowed=%v, the request got %d", self.Allowed, real.Code)
			case selfCode != http.StatusOK && selfCode != http.StatusNotFound:
				t.Fatalf("self review = %d", selfCode)
			}
		})
	}
}
