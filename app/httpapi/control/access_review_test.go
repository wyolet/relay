package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
)

// memUsers is a userFinder over accounts held in memory, keyed by id.
type memUsers map[string]*user.User

func (f memUsers) Get(_ context.Context, id string) (*user.User, error) { return f[id], nil }

func (f memUsers) ByUsername(_ context.Context, name string) (*user.User, error) {
	for _, u := range f {
		if u.Username == name {
			return u, nil
		}
	}
	return nil, nil
}

// reviewWorld is team t with projects p1 and p2. alice is a developer in p1, bob reaches p2 through group eng, carol is team-admin, and service account ci is a developer in p1.
type reviewWorld struct {
	h                    http.Handler
	sink                 *auditSink
	em                   *audit.Emitter
	actors               map[string]*actor.Actor
	users                memUsers
	team, p1, p2         string
	keyP1, keyP2, saID   string
	aliceBinding, roleID string
}

func reviewThing(id, name string, owner meta.Owner) *scopedThing {
	return &scopedThing{Meta: meta.Metadata{ID: id, Name: name, Owner: owner}}
}

func newReviewWorld(t *testing.T) *reviewWorld {
	t.Helper()
	on := true
	w := &reviewWorld{team: ids.New(), p1: ids.New(), p2: ids.New(), keyP1: ids.New(), keyP2: ids.New(), saID: ids.New()}
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	roleID := map[string]string{}
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &on
		roleID[r.Meta.Name] = r.Meta.ID
	}
	w.roleID = roleID["developer"]
	global := meta.Owner{Kind: meta.OwnerSystem}
	teamScope := meta.Owner{Kind: meta.OwnerTeam, ID: w.team}
	p1Scope, p2Scope := meta.Owner{Kind: meta.OwnerProject, ID: w.p1}, meta.Owner{Kind: meta.OwnerProject, ID: w.p2}

	w.users = memUsers{}
	addUser := func(name string, roles ...string) string {
		id := ids.New()
		w.users[id] = &user.User{ID: id, Username: name, Roles: roles}
		return id
	}
	alice, bob, carol, root := addUser("alice"), addUser("bob"), addUser("carol"), addUser("root", user.RoleAdmin)

	binding := func(name, roleID string, scope meta.Owner, subs ...rolebinding.Subject) *rolebinding.RoleBinding {
		b := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: scope}}
		b.Spec.RoleID, b.Spec.Scope, b.Spec.Subjects, b.Spec.Enabled = roleID, scope, subs, &on
		return b
	}
	aliceDev := binding("alice-dev", roleID["developer"], p1Scope, rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: alice})
	w.aliceBinding = aliceDev.Meta.ID
	tm := &team.Team{Meta: meta.Metadata{ID: w.team, Name: "t", Owner: global}}
	tm.Spec.Enabled = &on
	mkProject := func(id, name string) *project.Project {
		p := &project.Project{Meta: meta.Metadata{ID: id, Name: name, Owner: teamScope}}
		p.Spec.TeamID, p.Spec.Enabled = w.team, &on
		return p
	}
	eng := &group.Group{Meta: meta.Metadata{ID: ids.New(), Name: "eng", Owner: global}}
	eng.Spec.MemberIDs, eng.Spec.Enabled = []string{bob}, &on
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: w.saID, Name: "ci", Owner: p1Scope}}
	sa.Spec.ProjectID, sa.Spec.Enabled = w.p1, &on
	cat := catalogtest.Catalog{
		Teams:           []*team.Team{tm},
		Projects:        []*project.Project{mkProject(w.p1, "p1"), mkProject(w.p2, "p2")},
		Groups:          []*group.Group{eng},
		ServiceAccounts: []*serviceaccount.ServiceAccount{sa},
		Roles:           builtins,
		RoleBindings: []*rolebinding.RoleBinding{
			aliceDev,
			binding("eng-view", roleID["viewer"], p2Scope, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "eng"}),
			binding("carol-admin", roleID["team-admin"], teamScope, rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: carol}),
			binding("ci-dev", roleID["developer"], p1Scope, rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: w.saID}),
		},
	}.Load(t)

	signedIn := func(id string, groups ...string) *actor.Actor {
		u := w.users[id]
		return &actor.Actor{UserID: id, Username: u.Username, Roles: u.Roles, Subjects: appcatalog.UserSubjects(id, groups, nil)}
	}
	w.actors = map[string]*actor.Actor{
		"alice": signedIn(alice), "bob": signedIn(bob, "eng"), "carol": signedIn(carol), "root": signedIn(root),
		"token": {AdminToken: true, Username: "admin-token"},
	}

	w.sink = &auditSink{}
	w.em = audit.NewEmitter(w.sink, slog.New(slog.DiscardHandler))
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", w.actors))
	r.Use(audit.Middleware(w.em, nil))
	api := humachi.New(r, huma.DefaultConfig("access-review-test", "0"))
	az := audit.Authorizer{Inner: authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}}
	rows := kindIndex{}
	mount := func(plural, singular string, defaultOwner meta.OwnerKind, seed ...*scopedThing) {
		tmeta := func(v *scopedThing) *meta.Metadata { return &v.Meta }
		store := &memStore[scopedThing]{metaOf: tmeta, items: map[string]*scopedThing{}}
		for _, it := range seed {
			store.items[it.Meta.ID] = it
		}
		rows.add(registerKind[scopedThing](api, plural, singular, store, az, tmeta, nil, defaultOwner,
			listScanResolver[scopedThing](store, tmeta), nil, nil, nil, nil, nil, noSettings{}, false, nil, nil))
	}
	mount("teams", "team", meta.OwnerSystem, reviewThing(w.team, "t", global))
	mount("projects", "project", meta.OwnerTeam, reviewThing(w.p1, "p1", teamScope), reviewThing(w.p2, "p2", teamScope))
	mount("keys", "key", meta.OwnerProject, reviewThing(w.keyP1, "k1", p1Scope), reviewThing(w.keyP2, "k2", p2Scope))
	mount("service-accounts", "service-account", meta.OwnerProject, reviewThing(w.saID, "ci", p1Scope))
	rows.add(userRows(w.users))
	accessReviewer{authz: az, catalog: cat, users: w.users, rows: rows}.register(api, nil)
	w.h = r
	return w
}

func (w *reviewWorld) review(t *testing.T, who, body string) (int, accessReview) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/access-review", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Actor", who)
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	var out accessReview
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return rec.Code, out
}

func (w *reviewWorld) auditRows(t *testing.T) []audit.Event {
	t.Helper()
	w.em.Close()
	return w.sink.all()
}

func TestAccessReviewSelf(t *testing.T) {
	w := newReviewWorld(t)
	code, got := w.review(t, "alice", `{"action":"keys.get","resource":{"kind":"keys","name":"k1"}}`)
	if code != http.StatusOK || !got.Allowed || got.Reason != "binding" {
		t.Fatalf("alice keys.get k1 = %d %+v, want allowed by a binding", code, got)
	}
	if b := got.Binding; b == nil || b.ID != w.aliceBinding || b.Name != "alice-dev" || b.Role.Name != "developer" ||
		b.Scope.Name != "p1" || b.Subject.Kind != "user" || b.Subject.Name != "alice" {
		t.Fatalf("binding = %+v, want alice-dev granting developer at p1", got.Binding)
	}
	if len(got.Chain) != 3 || got.Chain[0].Name != "p1" || got.Chain[1].Name != "t" || got.Chain[2].Kind != "system" {
		t.Fatalf("chain = %+v, want p1, t, global", got.Chain)
	}
	if got.Subject.Name != "alice" || got.Resource.Name != "k1" || got.Resource.Owner == nil || got.Resource.Owner.Name != "p1" {
		t.Fatalf("subject %+v, resource %+v", got.Subject, got.Resource)
	}

	// Naming yourself is still yourself; a bare verb takes the kind's plural.
	code, got = w.review(t, "alice", `{"subject":{"kind":"user","name":"alice"},"action":"delete","resource":{"kind":"projects","name":"p1"}}`)
	if code != http.StatusOK || got.Allowed || got.Reason != "none" || got.Action != "projects.delete" {
		t.Fatalf("alice projects.delete p1 = %d %+v, want denied with no reason", code, got)
	}
	code, got = w.review(t, "alice", `{"action":"keys.list","resource":{"kind":"keys"}}`)
	if code != http.StatusOK || !got.Allowed || got.Reason != "rule" || got.Rule != "scoped-list" {
		t.Fatalf("alice keys.list = %d %+v, want the scoped-list rule", code, got)
	}
	code, got = w.review(t, "alice", `{"action":"keys.create","resource":{"kind":"keys","owner":{"kind":"project","name":"p1"}}}`)
	if code != http.StatusOK || !got.Allowed || got.Resource.Owner == nil || got.Resource.Owner.ID != w.p1 {
		t.Fatalf("alice keys.create in p1 = %d %+v", code, got)
	}
	if rows := w.auditRows(t); len(rows) != 0 {
		t.Fatalf("a self review wrote %d audit rows: %+v", len(rows), rows)
	}
}

// What the caller cannot see answers 404, as the resource's own routes do.
func TestAccessReviewHidesRowsTheCallerCannotSee(t *testing.T) {
	w := newReviewWorld(t)
	for _, body := range []string{
		`{"action":"keys.get","resource":{"kind":"keys","name":"k2"}}`,
		`{"action":"keys.create","resource":{"kind":"keys","owner":{"kind":"project","name":"p2"}}}`,
		`{"action":"keys.get","resource":{"kind":"keys","name":"nope"}}`,
		`{"action":"usage.read","resource":{"kind":"usage","name":"x"}}`,
	} {
		if code, got := w.review(t, "alice", body); code != http.StatusNotFound {
			t.Fatalf("%s = %d %+v, want 404", body, code, got)
		}
	}
}

func TestAccessReviewOfAnotherSubjectNeedsAdmin(t *testing.T) {
	w := newReviewWorld(t)
	for _, subject := range []string{`{"kind":"user","name":"bob"}`, `{"kind":"serviceaccount","name":"ci"}`, `{"kind":"user","name":"nobody"}`} {
		code, _ := w.review(t, "alice", `{"subject":`+subject+`,"action":"keys.get","resource":{"kind":"keys","name":"k1"}}`)
		if code != http.StatusForbidden {
			t.Fatalf("alice reviewing %s = %d, want 403", subject, code)
		}
	}
	rows := w.auditRows(t)
	if len(rows) != 3 {
		t.Fatalf("audit rows = %d, want one per refused review", len(rows))
	}
	for _, ev := range rows {
		if ev.Action != accessReviewAction || ev.Outcome.Status != audit.StatusDenied || ev.Outcome.Code != http.StatusForbidden {
			t.Fatalf("audit row = %+v, want a denied access review", ev)
		}
	}
}

func TestAccessReviewOfAnotherSubjectByAnAdmin(t *testing.T) {
	w := newReviewWorld(t)
	tests := []struct {
		who, subject, action, resource string
		allowed                        bool
		reason, binding                string
	}{
		{"root", `{"kind":"user","name":"bob"}`, "keys.get", `{"kind":"keys","name":"k2"}`, true, "binding", "eng-view"},
		{"root", `{"kind":"user","name":"bob"}`, "keys.delete", `{"kind":"keys","name":"k2"}`, false, "none", ""},
		{"root", `{"kind":"user","name":"carol"}`, "keys.delete", `{"kind":"keys","name":"k2"}`, true, "binding", "carol-admin"},
		{"token", `{"kind":"user","name":"alice"}`, "keys.get", `{"kind":"keys","name":"k2"}`, false, "none", ""},
		{"token", `{"kind":"serviceaccount","name":"ci"}`, "keys.get", `{"kind":"keys","name":"k1"}`, true, "binding", "ci-dev"},
		{"token", `{"kind":"user","name":"root"}`, "keys.delete", `{"kind":"keys","name":"k2"}`, true, "admin", ""},
	}
	for _, tt := range tests {
		code, got := w.review(t, tt.who, `{"subject":`+tt.subject+`,"action":"`+tt.action+`","resource":`+tt.resource+`}`)
		if code != http.StatusOK || got.Allowed != tt.allowed || got.Reason != tt.reason {
			t.Fatalf("%s reviewing %s %s = %d %+v, want allowed=%v reason=%s", tt.who, tt.subject, tt.action, code, got, tt.allowed, tt.reason)
		}
		binding := ""
		if got.Binding != nil {
			binding = got.Binding.Name
		}
		if binding != tt.binding {
			t.Fatalf("%s reviewing %s %s: binding %q, want %q", tt.who, tt.subject, tt.action, binding, tt.binding)
		}
	}
	rows := w.auditRows(t)
	if len(rows) != len(tests) {
		t.Fatalf("audit rows = %d, want one per review", len(rows))
	}
	ev := rows[0]
	if ev.Action != accessReviewAction || ev.Outcome.Status != audit.StatusAllowed || ev.Resource.Kind != "user" ||
		ev.Resource.Name != "bob" || ev.Resource.ID == "" || ev.Actor.Name != "root" {
		t.Fatalf("audit row = %+v, want root reviewing bob", ev)
	}
}

func TestAccessReviewUnknownSubjectOrResource(t *testing.T) {
	w := newReviewWorld(t)
	for _, body := range []string{
		`{"subject":{"kind":"user","name":"nobody"},"action":"keys.get","resource":{"kind":"keys","name":"k1"}}`,
		`{"subject":{"kind":"serviceaccount","name":"nobody"},"action":"keys.get","resource":{"kind":"keys","name":"k1"}}`,
		`{"subject":{"kind":"user","name":"bob"},"action":"keys.get","resource":{"kind":"keys","name":"nope"}}`,
		`{"subject":{"kind":"user","name":"bob"},"action":"keys.create","resource":{"kind":"keys","owner":{"kind":"project","name":"nope"}}}`,
	} {
		if code, got := w.review(t, "root", body); code != http.StatusNotFound {
			t.Fatalf("%s = %d %+v, want 404", body, code, got)
		}
	}
}

func TestAccessReviewUnderSingleUser(t *testing.T) {
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("access-review-single", "0"))
	accessReviewer{authz: audit.Authorizer{Inner: authz.AlwaysAllowAuthenticated{}}, rows: kindIndex{}}.register(api, nil)
	req := httptest.NewRequest(http.MethodPost, "/access-review", strings.NewReader(`{"action":"usage.read","resource":{"kind":"usage"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Actor", "alice")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d %s, want 501", rec.Code, rec.Body)
	}
}
