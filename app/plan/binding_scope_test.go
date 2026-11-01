package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
)

// ownedBy files a binding under owner, as the control API stores it.
func ownedBy(b *policybinding.PolicyBinding, owner meta.Owner) *policybinding.PolicyBinding {
	b.Meta.Owner, b.Spec.ProjectID = owner, ""
	if owner.Kind == meta.OwnerProject {
		b.Spec.ProjectID = owner.ID
	}
	return b
}

// scopeFixture is the basic fixture with a second model and a user holding a personal key: a policy granting only m1 shows on a route to the other model.
type scopeFixture struct {
	*basic
	u        *user.User
	userKey  *key.Key
	onlyM1   *policy.Policy
	personal *KeyPlan // the user key's plan at load
}

// newScopeFixture binds the user to the m1-only policy once per owner, each owner computed from the fixture.
func newScopeFixture(t *testing.T, owners ...func(*basic) meta.Owner) (*scopeFixture, *harness) {
	t.Helper()
	b := newBasic()
	other := b.w.model(b.prov, "other")
	for _, h := range []*host.Host{b.hostA, b.hostB} {
		b.w.bind(other, h)
		b.w.price(other, h)
	}
	f := &scopeFixture{basic: b, u: b.w.user()}
	f.userKey = b.w.userKey(f.u)
	f.onlyM1 = b.w.policy("only-m1-"+seq(), policy.Spec{Models: []string{"acme/m1"}})
	for _, o := range owners {
		ownedBy(b.w.bindPolicy(f.onlyM1, subject(rolebinding.SubjectUser, f.u.ID)), o(b))
	}
	h := b.w.start(t)
	f.personal = h.keyPlan(f.userKey)
	return f, h
}

func refusedOnPrincipal(r *Route) bool {
	return r.Refused != nil && r.Refused.Rung == limits.RungPrincipal && r.Refused.Code == CodeModelNotAllowed
}

// A project-owned binding on a user governs the user's tokens for that project and not the user's personal key; a team-owned one, the tokens for the team's projects.
func TestScopedUserBindingAppliesInsideItsOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner func(b *basic) meta.Owner
	}{
		{"project-owned", func(b *basic) meta.Owner { return meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID} }},
		{"team-owned", func(b *basic) meta.Owner { return meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := newScopeFixture(t, tc.owner)
			if r := f.personal.Caller.Routes.Get("other"); r.Refused != nil {
				t.Fatalf("personal key refused by a binding owned by a scope it is outside: %+v", r.Refused)
			}
			if f.personal.Caller.PrincipalRung().Policy != nil {
				t.Fatal("personal key folded a scoped binding into its own rung")
			}
			tok := h.tokenCaller(f.basic, f.u.ID)
			if r := tok.Routes.Get("other"); !refusedOnPrincipal(r) {
				t.Fatalf("token route to a model the scoped binding denies = %+v, want model_not_allowed on the principal rung", r.Refused)
			}
			if r := tok.Routes.Get("m1"); r.Refused != nil {
				t.Fatalf("token route to the granted model refused: %+v", r.Refused)
			}
		})
	}
}

// Binding a user from a project after load reaches the user's next token request in that project, and unbinding releases it, while the personal key stays ungoverned.
func TestScopedUserBindingWriteReachesTheToken(t *testing.T) {
	f, h := newScopeFixture(t)
	if r := h.tokenCaller(f.basic, f.u.ID).Routes.Get("other"); r.Refused != nil {
		t.Fatalf("token before any binding refused: %+v", r.Refused)
	}
	pb := ownedBy(&policybinding.PolicyBinding{
		Meta: md("pb-"+seq(), meta.Owner{}),
		Spec: policybinding.Spec{PolicyID: f.onlyM1.Meta.ID, Subjects: []rolebinding.Subject{subject(rolebinding.SubjectUser, f.u.ID)}},
	}, meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID})
	apply(h, pb)
	if r := h.tokenCaller(f.basic, f.u.ID).Routes.Get("other"); !refusedOnPrincipal(r) {
		t.Fatalf("token after the project bound the user = %+v, want model_not_allowed on the principal rung", r.Refused)
	}
	if r := h.keyPlan(f.userKey).Caller.Routes.Get("other"); r.Refused != nil {
		t.Fatalf("personal key refused after a project binding: %+v", r.Refused)
	}
	del(h, KindPolicyBinding, pb.Meta.ID)
	if r := h.tokenCaller(f.basic, f.u.ID).Routes.Get("other"); r.Refused != nil {
		t.Fatalf("token after the binding was deleted refused: %+v", r.Refused)
	}
}

// A token in another project, of another team, is outside a project- or team-owned binding.
func TestScopedUserBindingStaysOutOfOtherProjects(t *testing.T) {
	b := newBasic()
	other := b.w.model(b.prov, "other")
	b.w.bind(other, b.hostA)
	b.w.price(other, b.hostA)
	u := b.w.user()
	elsewhere := b.w.project(b.w.team())
	pol := b.w.policy("only-m1", policy.Spec{Models: []string{"acme/m1"}})
	ownedBy(b.w.bindPolicy(pol, subject(rolebinding.SubjectUser, u.ID)), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	h := b.w.start(t)

	root := h.holder.Load()
	tok := root.Projects.Lookup(elsewhere.Meta.ID).CallerFor(root.Users.Lookup(u.ID).Caller)
	if r := tok.Routes.Get("other"); r.Refused != nil {
		t.Fatalf("a token in another project took the binding: %+v", r.Refused)
	}
	if r := h.tokenCaller(b, u.ID).Routes.Get("other"); !refusedOnPrincipal(r) {
		t.Fatalf("a token in the owning project = %+v, want model_not_allowed on the principal rung", r.Refused)
	}
}

// A system-owned binding on a user governs everywhere: the personal key and every token.
func TestSystemUserBindingAppliesEverywhere(t *testing.T) {
	f, h := newScopeFixture(t, func(*basic) meta.Owner { return system })
	if r := f.personal.Caller.Routes.Get("other"); !refusedOnPrincipal(r) {
		t.Fatalf("personal key = %+v, want model_not_allowed on the principal rung", r.Refused)
	}
	if r := h.tokenCaller(f.basic, f.u.ID).Routes.Get("other"); !refusedOnPrincipal(r) {
		t.Fatalf("token = %+v, want model_not_allowed on the principal rung", r.Refused)
	}
}

// A system and a project binding both holding a token's request is two policies on one rung: the token refuses rather than pick one. The personal key, which only the system binding holds, still works.
func TestOverlappingUserBindingsRefuseTheToken(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	uk := b.w.userKey(u)
	pol := b.w.policy("p", policy.Spec{})
	ownedBy(b.w.bindPolicy(pol, subject(rolebinding.SubjectUser, u.ID)), system)
	ownedBy(b.w.bindPolicy(pol, subject(rolebinding.SubjectUser, u.ID)), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	h := b.w.start(t)

	if r := h.keyPlan(uk).Caller.Refused; r != nil {
		t.Fatalf("personal key refused: %+v", r)
	}
	if r := h.tokenCaller(b, u.ID).Refused; r == nil || r.Code != CodeDuplicateBinding || r.Rung != limits.RungPrincipal {
		t.Fatalf("token under two bindings = %+v, want duplicate_binding on the principal rung", r)
	}
}

// Two projects binding one user to one policy count in two buckets: a counter belongs to the binding's scope as well as to the subject and policy.
func TestScopedUserBindingCountsPerScope(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	second := b.w.project(b.team)
	rl := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	pol := b.w.policy("limited", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	ownedBy(b.w.bindPolicy(pol, subject(rolebinding.SubjectUser, u.ID)), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	ownedBy(b.w.bindPolicy(pol, subject(rolebinding.SubjectUser, u.ID)), meta.Owner{Kind: meta.OwnerProject, ID: second.Meta.ID})
	h := b.w.start(t)

	root := h.holder.Load()
	userPlan := root.Users.Lookup(u.ID).Caller
	first := root.Projects.Lookup(b.proj.Meta.ID).CallerFor(userPlan).Counters
	other := root.Projects.Lookup(second.Meta.ID).CallerFor(userPlan).Counters
	if len(first) != 1 || len(other) != 1 {
		t.Fatalf("counters: %d in the first project, %d in the second, want one each", len(first), len(other))
	}
	if first[0].ID == other[0].ID || !strings.Contains(first[0].ID, "@project:"+b.proj.Meta.ID) {
		t.Fatalf("counter ids %q and %q, want one per project scope", first[0].ID, other[0].ID)
	}
	if first[0].Group != "user:"+u.ID {
		t.Fatalf("counter group %q, want the user's", first[0].Group)
	}
}

// A binding owned by one project and naming another project's service account is ignored for it with an issue, however it was written; the account's own project binds it. Moving the account into the binding's project brings the binding into effect.
func TestBindingOutsideItsOwnersScopeNeverApplies(t *testing.T) {
	b := newBasic()
	elsewhere := b.w.project(b.team)
	pol := b.w.policy("only-m1", policy.Spec{Models: []string{"acme/m1"}})
	foreign := ownedBy(b.w.bindPolicy(pol, subject(rolebinding.SubjectServiceAccount, b.sa.Meta.ID)), meta.Owner{Kind: meta.OwnerProject, ID: elsewhere.Meta.ID})
	h := b.w.start(t)

	if rung := h.keyPlan(b.key).Caller.PrincipalRung(); rung.Policy != nil {
		t.Fatalf("service account governed by another project's binding: %+v", rung.Policy)
	}
	if !hasIssue(h.issues(KindPolicyBinding, foreign.Meta.ID), IssueSubjectOutOfScope) {
		t.Fatalf("issues = %+v, want %s", h.issues(KindPolicyBinding, foreign.Meta.ID), IssueSubjectOutOfScope)
	}

	moved := clone(b.sa)
	moved.Spec.ProjectID = elsewhere.Meta.ID
	moved.StampOwner()
	apply(h, moved)
	if rung := h.keyPlan(b.key).Caller.PrincipalRung(); rung.Policy == nil || rung.Policy.Policy.Meta.ID != pol.Meta.ID {
		t.Fatalf("service account inside the binding's project = %+v, want the bound policy", rung.Policy)
	}
	if hasIssue(h.issues(KindPolicyBinding, foreign.Meta.ID), IssueSubjectOutOfScope) {
		t.Fatal("the out-of-scope issue stayed after the account moved into scope")
	}
}
