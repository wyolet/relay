package plan

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
)

// personalPolicy is a policy owned by u.
func (w *world) personalPolicy(u *user.User, spec policy.Spec) *policy.Policy {
	p := w.policy("mine-"+seq(), spec)
	p.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: u.ID}
	return p
}

// A user's own binding of their own policy governs every request of the user: the personal key and a token in any project, counted in one bucket.
func TestUserOwnBindingAppliesToKeysAndTokens(t *testing.T) {
	f, h := newScopeFixture(t)
	rl := f.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	mine := f.w.personalPolicy(f.u, policy.Spec{Models: []string{"acme/m1"}, RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	apply(h, rl)
	apply(h, mine)
	apply(h, ownedBy(&policybinding.PolicyBinding{
		Meta: md("pb-"+seq(), meta.Owner{}),
		Spec: policybinding.Spec{PolicyID: mine.Meta.ID, Subjects: []rolebinding.Subject{subject(rolebinding.SubjectUser, f.u.ID)}},
	}, meta.Owner{Kind: meta.OwnerUser, ID: f.u.ID}))

	personal := h.keyPlan(f.userKey).Caller
	tok := h.tokenCaller(f.basic, f.u.ID)
	for name, cp := range map[string]*CallerPlan{"personal key": personal, "token": tok} {
		if r := cp.Routes.Get("other"); !refusedOnPrincipal(r) {
			t.Fatalf("%s route to a model the user's own policy denies = %+v, want model_not_allowed on the principal rung", name, r.Refused)
		}
		if r := cp.Routes.Get("m1"); r.Refused != nil {
			t.Fatalf("%s route to the granted model refused: %+v", name, r.Refused)
		}
	}
	if len(personal.Counters) != 1 || len(tok.Counters) != 1 || personal.Counters[0].ID != tok.Counters[0].ID {
		t.Fatalf("counters: personal %+v, token %+v, want the user's one bucket on both", personal.Counters, tok.Counters)
	}
}

// The user's own binding holds the user's one principal rung: beside a system binding it refuses everywhere, beside a project's binding it refuses that project's tokens and still governs the personal key.
func TestUserOwnBindingOverlapsEveryOtherScope(t *testing.T) {
	for _, tc := range []struct {
		name          string
		other         func(*basic) meta.Owner
		personalTwice bool
	}{
		{"system", func(*basic) meta.Owner { return system }, true},
		{"project", func(b *basic) meta.Owner { return meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBasic()
			u := b.w.user()
			uk := b.w.userKey(u)
			mine := b.w.personalPolicy(u, policy.Spec{})
			ownedBy(b.w.bindPolicy(mine, subject(rolebinding.SubjectUser, u.ID)), meta.Owner{Kind: meta.OwnerUser, ID: u.ID})
			ownedBy(b.w.bindPolicy(b.w.policy("p", policy.Spec{}), subject(rolebinding.SubjectUser, u.ID)), tc.other(b))
			h := b.w.start(t)

			r := h.keyPlan(uk).Caller.Refused
			if twice := r != nil && r.Code == CodeDuplicateBinding; twice != tc.personalTwice {
				t.Fatalf("personal key refusal = %+v, want duplicate_binding %v", r, tc.personalTwice)
			}
			if r := h.tokenCaller(b, u.ID).Refused; r == nil || r.Code != CodeDuplicateBinding || r.Rung != limits.RungPrincipal {
				t.Fatalf("token = %+v, want duplicate_binding on the principal rung", r)
			}
		})
	}
}
