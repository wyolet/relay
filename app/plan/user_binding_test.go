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

// A user's own binding of their own policy governs the user's personal requests, the relay keys, in the user's own bucket; a token, which acts inside a project, is not under it.
func TestUserOwnBindingGovernsPersonalRequests(t *testing.T) {
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
	if r := personal.Routes.Get("other"); !refusedOnPrincipal(r) {
		t.Fatalf("personal key route to a model the user's own policy denies = %+v, want model_not_allowed on the principal rung", r.Refused)
	}
	if r := personal.Routes.Get("m1"); r.Refused != nil {
		t.Fatalf("personal key route to the granted model refused: %+v", r.Refused)
	}
	if len(personal.Counters) != 1 || personal.Counters[0].Group != "user:"+f.u.ID || personal.Counters[0].SubjectID != "user:"+f.u.ID {
		t.Fatalf("personal counters = %+v, want one in the user's own bucket", personal.Counters)
	}
	tok := h.tokenCaller(f.basic, f.u.ID)
	if r := tok.Routes.Get("other"); r.Refused != nil {
		t.Fatalf("token refused by the user's own binding: %+v", r.Refused)
	}
	if len(tok.Counters) != 0 {
		t.Fatalf("token counters = %+v, want none", tok.Counters)
	}
}

// The user's own binding and a project's binding of the same user both stand: the personal key follows the user's policy, a token in the project the project's. Beside a system binding, which governs everywhere, the personal key refuses while the token follows the system's alone.
func TestUserOwnBindingBesideOtherScopes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		other         func(*basic) meta.Owner
		personalTwice bool
	}{
		{"project", func(b *basic) meta.Owner { return meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID} }, false},
		{"team", func(b *basic) meta.Owner { return meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID} }, false},
		{"system", func(*basic) meta.Owner { return system }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBasic()
			other := b.w.model(b.prov, "other")
			b.w.bind(other, b.hostA)
			b.w.price(other, b.hostA)
			u := b.w.user()
			uk := b.w.userKey(u)
			mine := b.w.personalPolicy(u, policy.Spec{Models: []string{"acme/m1"}})
			theirs := b.w.policy("only-other", policy.Spec{Models: []string{"acme/other"}})
			ownedBy(b.w.bindPolicy(mine, subject(rolebinding.SubjectUser, u.ID)), meta.Owner{Kind: meta.OwnerUser, ID: u.ID})
			ownedBy(b.w.bindPolicy(theirs, subject(rolebinding.SubjectUser, u.ID)), tc.other(b))
			h := b.w.start(t)

			personal := h.keyPlan(uk).Caller
			if tc.personalTwice {
				if r := personal.Refused; r == nil || r.Code != CodeDuplicateBinding || r.Rung != limits.RungPrincipal {
					t.Fatalf("personal key = %+v, want duplicate_binding on the principal rung", r)
				}
			} else if r := personal.Routes.Get("other"); !refusedOnPrincipal(r) || personal.Routes.Get("m1").Refused != nil {
				t.Fatalf("personal key: other %+v, m1 %+v; want the user's own policy (m1 only)", r.Refused, personal.Routes.Get("m1").Refused)
			}
			tok := h.tokenCaller(b, u.ID)
			if r := tok.Routes.Get("m1"); !refusedOnPrincipal(r) || tok.Routes.Get("other").Refused != nil {
				t.Fatalf("token: m1 %+v, other %+v; want the %s binding's policy (other only)", r.Refused, tok.Routes.Get("other").Refused, tc.name)
			}
		})
	}
}
