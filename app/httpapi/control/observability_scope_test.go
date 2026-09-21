package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/ids"
)

func TestScopeOf(t *testing.T) {
	tests := []struct {
		name           string
		authzr         authz.Authorizer
		who            *actor.Actor
		wantPrincipals []string
		unrestricted   bool
	}{
		{"single-user authorizer is unrestricted", authz.AlwaysAllowAuthenticated{}, scopeActors["alice"], nil, true},
		{"admin role is unrestricted", testRBAC(), scopeActors["root"], nil, true},
		{"admin token is unrestricted", testRBAC(), scopeActors["token"], nil, true},
		{"user is scoped to their own principal", testRBAC(), scopeActors["alice"], []string{"u-alice"}, false},
		{"user with no session identity is scoped to nothing", testRBAC(), &actor.Actor{}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := actor.WithActor(context.Background(), tt.who)
			sc, err := scopeOf(ctx, tt.authzr, nil, "usage")
			if err != nil {
				t.Fatal(err)
			}
			if sc.unrestricted != tt.unrestricted {
				t.Fatalf("unrestricted = %v, want %v", sc.unrestricted, tt.unrestricted)
			}
			if len(sc.principalIDs) != len(tt.wantPrincipals) {
				t.Fatalf("principalIDs = %v, want %v", sc.principalIDs, tt.wantPrincipals)
			}
			for i := range sc.principalIDs {
				if sc.principalIDs[i] != tt.wantPrincipals[i] {
					t.Fatalf("principalIDs = %v, want %v", sc.principalIDs, tt.wantPrincipals)
				}
			}
		})
	}
}

func TestScopeEventQuery(t *testing.T) {
	t.Run("unrestricted leaves the query alone", func(t *testing.T) {
		q := usagelog.EventQuery{}
		if !scopeEventQuery(&q, readScope{unrestricted: true}) {
			t.Fatal("want true")
		}
		if len(q.ScopeProjectID) != 0 || len(q.ScopePrincipalID) != 0 {
			t.Fatalf("query narrowed: %+v", q)
		}
	})
	t.Run("scope rides the query as a disjunction", func(t *testing.T) {
		q := usagelog.EventQuery{}
		sc := readScope{projectIDs: []string{"p-1"}, principalIDs: []string{"u-1", "u-2"}}
		if !scopeEventQuery(&q, sc) {
			t.Fatal("want true")
		}
		if len(q.ScopeProjectID) != 1 || len(q.ScopePrincipalID) != 2 {
			t.Fatalf("query = %+v, want the scope carried verbatim", q)
		}
	})
	t.Run("a caller filter is not widened by the scope", func(t *testing.T) {
		q := usagelog.EventQuery{RelayKeyHash: []string{"h-foreign"}}
		if !scopeEventQuery(&q, readScope{principalIDs: []string{"u-1"}}) {
			t.Fatal("want true")
		}
		// The caller's own filter stays; the scope is ANDed on top of it.
		if len(q.RelayKeyHash) != 1 || q.RelayKeyHash[0] != "h-foreign" {
			t.Fatalf("RelayKeyHash = %v, want the caller filter untouched", q.RelayKeyHash)
		}
		if len(q.ScopePrincipalID) != 1 {
			t.Fatalf("ScopePrincipalID = %v", q.ScopePrincipalID)
		}
	})
	t.Run("an empty scope matches nothing", func(t *testing.T) {
		q := usagelog.EventQuery{}
		if scopeEventQuery(&q, readScope{}) {
			t.Fatal("want false")
		}
	})
}

// fakeUsageReader records the query it was handed; calls counts invocations
// so tests can assert the scoped-out short-circuit never reaches the store.
type fakeUsageReader struct {
	calls  int
	last   usagelog.EventQuery
	events []usagelog.Event
}

func (f *fakeUsageReader) Events(_ context.Context, q usagelog.EventQuery) ([]usagelog.Event, error) {
	f.calls++
	f.last = q
	return f.events, nil
}

func (f *fakeUsageReader) Summary(_ context.Context, q usagelog.SummaryQuery) (usagelog.SummaryResult, error) {
	f.calls++
	f.last = q.EventQuery
	return usagelog.SummaryResult{Rows: []usagelog.SummaryRow{}}, nil
}

func (f *fakeUsageReader) TimeSeries(_ context.Context, q usagelog.TimeSeriesQuery) (usagelog.TimeSeriesResult, error) {
	f.calls++
	f.last = q.EventQuery
	return usagelog.TimeSeriesResult{Rows: []usagelog.TimeSeriesRow{}}, nil
}

// newUsageHarness mounts the usage + logs read endpoints behind the actor-
// injecting middleware from scope_test.go. Stores is nil, so a scoped
// caller resolves to zero owned keys (the fail-closed path).
func newUsageHarness(t *testing.T, authzr authz.Authorizer, reader *fakeUsageReader) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("usage-scope-test", "0"))
	d := Deps{Authz: authzr, UsageReader: reader}
	registerUsage(api, d, nil)
	registerLogs(api, d, nil)
	return r
}

func TestUsageReadScoping(t *testing.T) {
	t.Run("scoped user reads only their own principal's events", func(t *testing.T) {
		reader := &fakeUsageReader{events: []usagelog.Event{{RequestID: "r-1"}}}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "alice", http.MethodGet, "/usage/events", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if reader.calls != 1 {
			t.Fatalf("reader called %d times, want 1", reader.calls)
		}
		if len(reader.last.ScopePrincipalID) != 1 || reader.last.ScopePrincipalID[0] != "u-alice" {
			t.Fatalf("ScopePrincipalID = %v, want [u-alice]", reader.last.ScopePrincipalID)
		}
	})

	t.Run("a caller with no principal at all is scoped out before the store", func(t *testing.T) {
		reader := &fakeUsageReader{events: []usagelog.Event{{RequestID: "r-1"}}}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "nobody", http.MethodGet, "/usage/events", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		var out struct {
			Events []usagelog.Event `json:"events"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Events) != 0 {
			t.Fatalf("events = %v, want none", out.Events)
		}
		if reader.calls != 0 {
			t.Fatalf("reader called %d times, want 0", reader.calls)
		}
	})

	t.Run("admin reads unscoped", func(t *testing.T) {
		reader := &fakeUsageReader{}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "root", http.MethodGet, "/usage/events", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if reader.calls != 1 || len(reader.last.RelayKeyHash) != 0 {
			t.Fatalf("calls=%d filter=%v, want one unfiltered call", reader.calls, reader.last.RelayKeyHash)
		}
	})

	t.Run("single-user authorizer reads unscoped", func(t *testing.T) {
		reader := &fakeUsageReader{}
		h := newUsageHarness(t, authz.AlwaysAllowAuthenticated{}, reader)
		w := scopeReq(t, h, "alice", http.MethodGet, "/usage/summary", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if reader.calls != 1 || len(reader.last.RelayKeyHash) != 0 {
			t.Fatalf("calls=%d filter=%v, want one unfiltered call", reader.calls, reader.last.RelayKeyHash)
		}
	})

	t.Run("scoped user cannot fetch a foreign log record", func(t *testing.T) {
		reader := &fakeUsageReader{events: []usagelog.Event{{RequestID: "r-1", RelayKeyHash: "hash-bob"}}}
		h := newUsageHarness(t, testRBAC(), reader)
		if w := scopeReq(t, h, "alice", http.MethodGet, "/logs/r-1", ""); w.Code != 404 {
			t.Fatalf("status = %d, want 404: %s", w.Code, w.Body)
		}
		if w := scopeReq(t, h, "root", http.MethodGet, "/logs/r-1", ""); w.Code != 200 {
			t.Fatalf("admin status = %d, want 200: %s", w.Code, w.Body)
		}
	})
}

// Own traffic is matched by principal, so a key's rotation — which changes
// its hash and eventually retires the old one — never drops the caller's own
// events out of their scope.
func TestScopeOfCoversOwnTrafficAcrossKeyRotation(t *testing.T) {
	ctx := actor.WithActor(context.Background(), scopeActors["alice"])
	sc, err := scopeOf(ctx, testRBAC(), nil, "usage")
	if err != nil {
		t.Fatal(err)
	}
	rotated := usagelog.Event{RequestID: "r-1", RelayKeyHash: "hash-brand-new", PrincipalID: "u-alice"}
	if !sc.allows(rotated) {
		t.Fatalf("scope %+v rejects the caller's own event after a rotation", sc)
	}
	foreign := usagelog.Event{RequestID: "r-2", RelayKeyHash: "hash-bob", PrincipalID: "u-bob"}
	if sc.allows(foreign) {
		t.Fatalf("scope %+v admits another principal's event", sc)
	}
}

// viewerFixture builds a two-project catalog with a viewer role bound at
// project p1 only, for exercising scopeOf's project-scan and per-kind
// branches, which an empty catalog (testRBAC) can never reach.
func viewerFixture(t *testing.T) (cat *appcatalog.Catalog, viewer *actor.Actor, p1ID string) {
	t.Helper()
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	yes := true
	var viewerRoleID string
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &yes
		if r.Meta.Name == "viewer" {
			viewerRoleID = r.Meta.ID
		}
	}
	if viewerRoleID == "" {
		t.Fatal("no built-in viewer role")
	}

	teamID, p1, p2, viewerUserID := ids.New(), ids.New(), ids.New(), ids.New()
	tm := &team.Team{Meta: meta.Metadata{ID: teamID, Name: "t1", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tm.Spec.Enabled = &yes
	proj1 := &project.Project{Meta: meta.Metadata{ID: p1, Name: "p1", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}}
	proj1.Spec.TeamID = teamID
	proj1.Spec.Enabled = &yes
	proj2 := &project.Project{Meta: meta.Metadata{ID: p2, Name: "p2", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}}
	proj2.Spec.TeamID = teamID
	proj2.Spec.Enabled = &yes

	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "viewer-p1", Owner: meta.Owner{Kind: meta.OwnerProject, ID: p1}}}
	rb.Spec.RoleID = viewerRoleID
	rb.Spec.Scope = meta.Owner{Kind: meta.OwnerProject, ID: p1}
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: viewerUserID}}
	rb.Spec.Enabled = &yes

	cat = appcatalog.New(
		tokenList[provider.Provider]{}, tokenList[host.Host]{}, tokenList[policy.Policy]{},
		tokenList[model.Model]{}, tokenList[hostkey.HostKey]{}, tokenList[ratelimit.RateLimit]{},
		tokenList[key.Key]{}, tokenList[pricing.Pricing]{}, tokenList[binding.Binding]{},
	)
	cat.UseTenancy(
		tokenList[team.Team]{tm}, tokenList[project.Project]{proj1, proj2},
		tokenList[serviceaccount.ServiceAccount]{}, tokenList[group.Group]{},
		tokenList[role.Role](builtins), tokenList[rolebinding.RoleBinding]{rb},
		tokenList[policybinding.PolicyBinding]{},
	)
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	viewer = &actor.Actor{UserID: viewerUserID, Subjects: appcatalog.UserSubjects(viewerUserID, nil, nil)}
	return cat, viewer, p1
}

func TestScopeOfProjectBranchResolvesOnlyTheBoundProject(t *testing.T) {
	cat, viewer, p1 := viewerFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	ctx := actor.WithActor(context.Background(), viewer)

	sc, err := scopeOf(ctx, rbac, cat, "usage")
	if err != nil {
		t.Fatal(err)
	}
	if sc.unrestricted {
		t.Fatal("a viewer bound at one project should not resolve unrestricted")
	}
	if len(sc.projectIDs) != 1 || sc.projectIDs[0] != p1 {
		t.Fatalf("projectIDs = %v, want exactly [%s]", sc.projectIDs, p1)
	}
}

// The viewer role grants usage.{get,read} but not logs — scopeOf must
// resolve the two kinds independently rather than sharing one scope.
func TestScopeOfLogsIsNotGrantedByAUsageOnlyRole(t *testing.T) {
	cat, viewer, p1 := viewerFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	ctx := actor.WithActor(context.Background(), viewer)

	usageSc, err := scopeOf(ctx, rbac, cat, "usage")
	if err != nil {
		t.Fatal(err)
	}
	if len(usageSc.projectIDs) != 1 || usageSc.projectIDs[0] != p1 {
		t.Fatalf("usage scope = %+v, want the bound project %s", usageSc, p1)
	}

	logsSc, err := scopeOf(ctx, rbac, cat, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if logsSc.unrestricted || len(logsSc.projectIDs) != 0 {
		t.Fatalf("logs scope = %+v, want empty (viewer role has no logs grant)", logsSc)
	}
}
