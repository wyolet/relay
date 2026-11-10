package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/engine"
)

// reach is basic plus model m2, served at host-b only, and a pool per host.
type reach struct {
	*basic
	m2           *model.Model
	poolA, poolB *pool.Pool
}

func newReach() *reach {
	b := newBasic()
	r := &reach{basic: b, m2: b.w.model(b.prov, "m2")}
	b.w.bind(r.m2, b.hostB)
	r.poolA, r.poolB = b.w.pool(b.hostA, b.a1), b.w.pool(b.hostB, b.b1)
	return r
}

func TestRateLimitIssues(t *testing.T) {
	r := newReach()
	off := r.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	off.Spec.Enabled = boolp(false)
	on := r.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	pol := r.w.policy("limited", policy.Spec{Models: []string{"acme/m1"}, RateLimits: []policy.RateLimitLink{
		{RateLimitID: off.Meta.ID},
		{RateLimitID: on.Meta.ID, Models: []string{"acme/m2"}},
		{RateLimitID: on.Meta.ID, Models: []string{"acme"}},
	}})
	h := r.w.start(t)
	is := h.issues(KindPolicy, pol.Meta.ID)

	if i := issueAt(is, IssueRateLimitDisabled, "spec.rateLimits[0]"); i == nil || i.Target.ID != off.Meta.ID || !strings.Contains(i.Message, off.Meta.Name) {
		t.Errorf("issues = %+v, want %s on the disabled link", is, IssueRateLimitDisabled)
	}
	if i := issueAt(is, IssueRateLimitScopeOutsideModels, "spec.rateLimits[1].models"); i == nil || i.Target.ID != on.Meta.ID {
		t.Errorf("issues = %+v, want %s on the link scoped to m2", is, IssueRateLimitScopeOutsideModels)
	}
	if issueAt(is, IssueRateLimitScopeOutsideModels, "spec.rateLimits[2].models") != nil {
		t.Errorf("a scope overlapping the models is reported: %+v", is)
	}
}

func TestModelRefIssues(t *testing.T) {
	r := newReach()
	m3 := r.w.model(r.prov, "m3")
	m3.Spec.Enabled = boolp(false)
	r.w.bind(m3, r.hostA)
	some := r.w.policy("some", policy.Spec{Models: []string{"acme/m1", "acme/m2", "acme/m3"}, PoolIDs: []string{r.poolA.Meta.ID}})
	none := r.w.policy("none", policy.Spec{Models: []string{"acme/m2"}, PoolIDs: []string{r.poolA.Meta.ID}})
	unlinked := r.w.policy("unlinked", policy.Spec{Models: []string{"acme/m2", "acme/m3"}})
	h := r.w.start(t)

	is := h.issues(KindPolicy, some.Meta.ID)
	if i := issueAt(is, IssueModelRefUnreachable, "spec.models[1]"); i == nil || i.Severity != engine.SeverityWarning || !strings.Contains(i.Message, r.hostA.Meta.Name) {
		t.Errorf("some: %+v, want a %s warning on m2 naming host-a", is, IssueModelRefUnreachable)
	}
	if i := issueAt(is, IssueModelRefDisabled, "spec.models[2]"); i == nil || i.Target.ID != m3.Meta.ID {
		t.Errorf("some: %+v, want %s on m3", is, IssueModelRefDisabled)
	}
	if issueAt(is, IssueModelRefUnreachable, "spec.models[0]") != nil || issueAt(is, IssueModelRefUnreachable, "spec.models[2]") != nil {
		t.Errorf("some: a reachable or disabled entry reported unreachable: %+v", is)
	}
	if i := issueAt(h.issues(KindPolicy, none.Meta.ID), IssueModelRefUnreachable, "spec.models[0]"); i == nil || i.Severity != engine.SeverityError {
		t.Errorf("none: %+v, want a %s error", h.issues(KindPolicy, none.Meta.ID), IssueModelRefUnreachable)
	}
	is = h.issues(KindPolicy, unlinked.Meta.ID)
	if hasIssue(is, IssueModelRefUnreachable) || !hasIssue(is, IssueModelRefDisabled) {
		t.Errorf("unlinked: %+v, want %s only", is, IssueModelRefDisabled)
	}

	// Binding m2 at host-a makes it reachable.
	apply(h, r.w.bind(r.m2, r.hostA))
	if hasIssue(h.issues(KindPolicy, none.Meta.ID), IssueModelRefUnreachable) {
		t.Errorf("none after binding m2 at host-a: %+v", h.issues(KindPolicy, none.Meta.ID))
	}
}

func TestPoolOutsideModels(t *testing.T) {
	r := newReach()
	pol := r.w.policy("m2-only", policy.Spec{Models: []string{"acme/m2"}, PoolIDs: []string{r.poolA.Meta.ID, r.poolB.Meta.ID}})
	all := r.w.policy("everything", policy.Spec{PoolIDs: []string{r.poolA.Meta.ID}})
	h := r.w.start(t)
	is := h.issues(KindPolicy, pol.Meta.ID)
	if i := issueAt(is, IssuePoolOutsideModels, "spec.poolIds[0]"); i == nil || i.Target != (engine.Ref{Kind: KindPool, ID: r.poolA.Meta.ID}) || !strings.Contains(i.Message, r.hostA.Meta.Name) {
		t.Errorf("issues = %+v, want %s on pool A", is, IssuePoolOutsideModels)
	}
	if issueAt(is, IssuePoolOutsideModels, "spec.poolIds[1]") != nil {
		t.Errorf("pool B serves m2 but is reported: %+v", is)
	}
	if is := h.issues(KindPolicy, all.Meta.ID); len(is) > 0 {
		t.Errorf("a policy allowing every model: %+v", is)
	}
}

// An unservable model is reported only while an enabled policy grants it, once per granting policy, naming it.
func TestModelUnservable(t *testing.T) {
	r := newReach()
	m2b := r.w.bindings[len(r.w.bindings)-1]
	m2b.Spec.Enabled = boolp(false)
	unbound := r.w.model(r.prov, "unbound")
	byName := r.w.policy("names-m2", policy.Spec{Models: []string{"acme/m2", "acme/unbound"}})
	byProvider := r.w.policy("all-acme", policy.Spec{Models: []string{"acme"}})
	off := r.w.policy("off", policy.Spec{Models: []string{"acme/m2"}, Enabled: boolp(false)})
	h := r.w.start(t)
	is := h.issues(KindModel, r.m2.Meta.ID)
	if len(is) != 2 {
		t.Fatalf("m2 issues = %+v, want one per enabled granting policy", is)
	}
	for _, i := range is {
		if i.Code != IssueModelUnservable || i.Severity != engine.SeverityError || !strings.Contains(i.Message, r.hostB.Meta.Name) || i.Target.Kind != KindPolicy || i.Target.ID == off.Meta.ID {
			t.Errorf("m2 issue = %+v, want %s naming host-b and an enabled policy", i, IssueModelUnservable)
		}
	}
	if !strings.Contains(is[0].Message+is[1].Message, byName.Meta.Name) || !strings.Contains(is[0].Message+is[1].Message, byProvider.Meta.Name) {
		t.Errorf("m2 issues = %+v, want both granting policies named", is)
	}

	del(h, KindPolicy, byName.Meta.ID)
	del(h, KindPolicy, byProvider.Meta.ID)
	if is := h.issues(KindModel, r.m2.Meta.ID); len(is) > 0 {
		t.Fatalf("m2 granted by no enabled policy: %+v", is)
	}
	apply(h, r.w.policy("again", policy.Spec{Models: []string{"acme/m2"}}))
	if is := h.issues(KindModel, unbound.Meta.ID); len(is) > 0 {
		t.Errorf("a model with no binding: %+v", is)
	}
	if is := h.issues(KindModel, r.m1.Meta.ID); len(is) > 0 {
		t.Errorf("m1 is served: %+v", is)
	}

	on := clone(m2b)
	on.Spec.Enabled = nil
	apply(h, on)
	if is := h.issues(KindModel, r.m2.Meta.ID); len(is) > 0 {
		t.Errorf("m2 after enabling its binding: %+v", is)
	}
	offHost := clone(r.hostB)
	offHost.Spec.Enabled = boolp(false)
	apply(h, offHost)
	if !hasIssue(h.issues(KindModel, r.m2.Meta.ID), IssueModelUnservable) {
		t.Errorf("m2 with its only host disabled: %+v", h.issues(KindModel, r.m2.Meta.ID))
	}
}

// A key naming no account whose principal has no account of its own names the principal, not an empty account id.
func TestKeyIssue_NoOwnAccount(t *testing.T) {
	w := &world{}
	p := w.project(w.team())
	sa := &serviceaccount.ServiceAccount{Meta: md("lonely-"+seq(), meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}), Spec: serviceaccount.Spec{ProjectID: p.Meta.ID}}
	w.sas = append(w.sas, sa)
	k := &key.Key{Meta: md("key-"+seq(), system), Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, KeyHash: keyHash("lonely")}}
	w.keys = append(w.keys, k)
	h := w.start(t)
	i := issueAt(h.issues(KindKey, k.Meta.ID), IssueRefMissing, "spec.accountId")
	if i == nil || strings.Contains(i.Message, "account  ") || !strings.Contains(i.Message, "service account "+sa.Meta.Name) || i.Target != (engine.Ref{}) {
		t.Errorf("issue = %+v, want one naming %s and no target", i, sa.Meta.Name)
	}
}
