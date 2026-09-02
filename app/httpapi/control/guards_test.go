// guards_test.go covers the mutation guards registerKind runs before a
// write: the ones that decide on the incoming row alone. The guards that
// read a sibling store (checkPolicyRefVisible, checkTeamRefVisible,
// checkRoleRefVisible, …) reach concrete *X.Store values with no fake seam
// and are exercised in policyref_integration_test.go instead; what is
// unit-testable of those here is the visibility rule they delegate to.
package control

import (
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// A HostKey's credential is rotated through its own endpoint so the old
// hash keeps a grace window; a value smuggled in on PUT would replace it
// with no grace and no rotation to audit.
func TestGuardHostKeyRefusesAValueOnUpdate(t *testing.T) {
	mk := func(kind hostkey.ValueKind, value string) *hostkey.HostKey {
		k := &hostkey.HostKey{}
		k.Meta = meta.Metadata{ID: meta.NewID(), Name: "k"}
		k.Spec.HostID = meta.NewID()
		k.Spec.ValueFrom = hostkey.ValueFrom{Kind: kind}
		k.Spec.Value = value
		return k
	}
	existing := mk(hostkey.ValueKindStored, "")
	existing.Resolved = "sk-current"

	for _, tc := range []struct {
		name     string
		action   string
		existing *hostkey.HostKey
		incoming *hostkey.HostKey
		wantErr  bool
	}{
		{name: "create carries the value", action: "create", incoming: mk(hostkey.ValueKindStored, "sk-new")},
		{name: "update rotating a stored value", action: "update", existing: existing,
			incoming: mk(hostkey.ValueKindStored, "sk-new"), wantErr: true},
		{name: "update rotating an oauth blob", action: "update", existing: existing,
			incoming: mk(hostkey.ValueKindOAuth, "{}"), wantErr: true},
		{name: "update resending the current value", action: "update", existing: existing,
			incoming: mk(hostkey.ValueKindStored, "sk-current")},
		{name: "update with no value at all", action: "update", existing: existing,
			incoming: mk(hostkey.ValueKindStored, "")},
		{name: "env-ref update is never a rotation", action: "update", existing: existing,
			incoming: mk(hostkey.ValueKindEnv, "")},
		{name: "delete ignores the value", action: "delete", existing: existing, incoming: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := guardHostKey(Deps{})(context.Background(), tc.action, tc.existing, tc.incoming)
			if (err != nil) != tc.wantErr {
				t.Fatalf("guardHostKey = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// Spec is the source of truth for a scoped row's owner: a caller who sends
// a mismatched owner must not end up with a row filed under it.
func TestGuardsReDeriveTheOwnerFromSpec(t *testing.T) {
	teamID, projectID := meta.NewID(), meta.NewID()
	wrong := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}

	proj := &project.Project{}
	proj.Meta = meta.Metadata{ID: meta.NewID(), Name: "p", Owner: wrong}
	proj.Spec.TeamID = teamID

	sa := &serviceaccount.ServiceAccount{}
	sa.Meta = meta.Metadata{ID: meta.NewID(), Name: "sa", Owner: wrong}
	sa.Spec.ProjectID = projectID

	rb := &rolebinding.RoleBinding{}
	rb.Meta = meta.Metadata{ID: meta.NewID(), Name: "rb", Owner: wrong}
	rb.Spec.RoleID = meta.NewID()
	rb.Spec.Scope = meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "eng"}}

	pb := &policybinding.PolicyBinding{}
	pb.Meta = meta.Metadata{ID: meta.NewID(), Name: "pb", Owner: wrong}
	pb.Spec.ProjectID = projectID
	pb.Spec.PolicyID = meta.NewID()
	pb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}}

	for _, tc := range []struct {
		name  string
		run   func() error
		owner func() meta.Owner
		want  meta.Owner
	}{
		{"project takes its team",
			func() error { return guardProject(Deps{})(context.Background(), "create", nil, proj) },
			func() meta.Owner { return proj.Meta.Owner },
			meta.Owner{Kind: meta.OwnerTeam, ID: teamID}},
		{"service account takes its project",
			func() error { return guardServiceAccount(Deps{})(context.Background(), "create", nil, sa) },
			func() meta.Owner { return sa.Meta.Owner },
			meta.Owner{Kind: meta.OwnerProject, ID: projectID}},
		{"role binding takes its scope",
			func() error { return guardRoleBinding(Deps{})(context.Background(), "create", nil, rb) },
			func() meta.Owner { return rb.Meta.Owner },
			meta.Owner{Kind: meta.OwnerTeam, ID: teamID}},
		{"policy binding takes its project",
			func() error { return guardPolicyBinding(Deps{})(context.Background(), "create", nil, pb) },
			func() meta.Owner { return pb.Meta.Owner },
			meta.Owner{Kind: meta.OwnerProject, ID: projectID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err != nil {
				t.Fatalf("guard: %v", err)
			}
			if got := tc.owner(); got != tc.want {
				t.Fatalf("owner = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The default is applied to an absent priority, never to an explicit one.
func TestGuardPolicyBindingKeepsAnExplicitPriority(t *testing.T) {
	mk := func(priority int) *policybinding.PolicyBinding {
		b := &policybinding.PolicyBinding{}
		b.Meta = meta.Metadata{ID: meta.NewID(), Name: "pb"}
		b.Spec.ProjectID = meta.NewID()
		b.Spec.PolicyID = meta.NewID()
		b.Spec.Priority = priority
		return b
	}
	for _, tc := range []struct{ in, want int }{
		{0, policybinding.DefaultPriority},
		{50, 50},
		{policybinding.DefaultPriority, policybinding.DefaultPriority},
	} {
		b := mk(tc.in)
		if err := guardPolicyBinding(Deps{})(context.Background(), "create", nil, b); err != nil {
			t.Fatalf("guard: %v", err)
		}
		if b.Spec.Priority != tc.want {
			t.Errorf("priority %d => %d, want %d", tc.in, b.Spec.Priority, tc.want)
		}
	}
}

// The built-in check runs before the license check, so an expired license
// never masks the reason a built-in edit was refused.
func TestGuardRoleProtectsExistingBuiltins(t *testing.T) {
	builtin := &role.Role{Meta: meta.Metadata{ID: meta.NewID(), Name: "developer",
		Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	for _, action := range []string{"update", "delete"} {
		err := guardRole(Deps{})(context.Background(), action, builtin, customRole("release-manager"))
		if got := statusOf(t, err); got != 403 {
			t.Errorf("%s of a built-in = status %d, want 403", action, got)
		}
	}
}

// Team, Group and Role default to a system owner, so creating one is an
// admin operation — the personal-row rule must never carry a plain user
// through a tenancy create.
func TestTenancyCreateIsNeverAPersonalRow(t *testing.T) {
	for _, tc := range []struct {
		kind string
		h    http.Handler
		body string
	}{
		{"teams", newTenancyHarness(t, "teams", "team",
			func(v *team.Team) *meta.Metadata { return &v.Meta },
			func(v *team.Team) error { return v.Validate() }),
			`{"metadata":{"name":"platform","displayName":"Platform"},"spec":{}}`},
		{"groups", newTenancyHarness(t, "groups", "group",
			func(v *group.Group) *meta.Metadata { return &v.Meta },
			func(v *group.Group) error { return v.Validate() }),
			`{"metadata":{"name":"eng","displayName":"Eng"},"spec":{}}`},
		{"roles", newTenancyHarness(t, "roles", "role",
			func(v *role.Role) *meta.Metadata { return &v.Meta },
			func(v *role.Role) error { return v.Validate() }),
			`{"metadata":{"name":"release-manager","displayName":"RM"},` +
				`"spec":{"rules":[{"kinds":["keys"],"verbs":["get"]}]}}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if w := scopeReq(t, tc.h, "alice", http.MethodPost, "/"+tc.kind, tc.body); w.Code != http.StatusForbidden {
				t.Fatalf("plain user create = %d, want 403: %s", w.Code, w.Body)
			}
			if w := scopeReq(t, tc.h, "root", http.MethodPost, "/"+tc.kind, tc.body); w.Code != http.StatusCreated {
				t.Fatalf("admin create = %d, want 201: %s", w.Code, w.Body)
			}
		})
	}
}

// newTenancyHarness mounts one system-owned kind behind the scope
// harness's actor-injecting middleware.
func newTenancyHarness[T any](t *testing.T, plural, singular string,
	metaOf func(*T) *meta.Metadata, validate func(*T) error,
) http.Handler {
	t.Helper()
	store := &memStore[T]{metaOf: metaOf, items: map[string]*T{}}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig(plural+"-guards-test", "0"))
	registerKind[T](
		api, plural, singular, store, testRBAC(), metaOf, validate,
		meta.OwnerSystem, listScanResolver[T](store, metaOf),
		nil, nil, nil, nil, nil, noSettings{}, false, nil, nil,
	)
	return r
}

// checkRoleRefVisible reads its row from a concrete store, but the rule it
// applies is Visible: a scoped caller holds no binding at the global scope
// a built-in Role lives in and could otherwise never name one in a binding.
func TestSystemOwnedRolesAreVisibleToAScopedCaller(t *testing.T) {
	rbac := authz.RBAC{Snap: func() authz.Snapshot {
		return appcatalog.Build(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	}}
	ctx := actor.WithActor(context.Background(),
		&actor.Actor{UserID: "u-alice", Subjects: []string{"user:u-alice"}})

	if !rbac.Visible(ctx, "role", meta.NewID(), meta.Owner{Kind: meta.OwnerSystem}) {
		t.Error("a system-owned role is not visible to a scoped caller")
	}
	if rbac.Visible(ctx, "role", meta.NewID(), meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}) {
		t.Error("another user's custom role is visible")
	}
}

// D65: the binder must already hold everything the bound role grants at
// that scope, or a team-admin can bind `admin` to himself.
func TestGuardRoleBindingRefusesEscalation(t *testing.T) {
	t.Skip("D65 not implemented: guardRoleBinding never compares the bound role's rules against the binder's own grants, so a team-admin can bind the built-in admin role at their own team")
}

// D67: an upgraded deployment carries no bindings at all, and a list there
// must answer 200 with the caller's own rows.
func TestZeroBindingListAnswersWithOwnRows(t *testing.T) {
	t.Skip("D67 not implemented: keys.list with no owner requires a granting binding, so a binding-less user gets 403 instead of an empty 200")
}
