package plan

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// issueAt returns the issue of code at field, nil when there is none.
func issueAt(is []engine.Issue, code, field string) *engine.Issue {
	for i := range is {
		if is[i].Code == code && is[i].Field == field {
			return &is[i]
		}
	}
	return nil
}

// A pool names each listed key and account that adds no usable key, and a policy whose pools all serve nothing says so; the issues go once a key is usable again.
func TestPoolMemberUnusable_DisabledAndUnresolvedKeys(t *testing.T) {
	b := newBasic()
	b.a1.Spec.Enabled = boolp(false)
	b.a2.Status.Unresolved = &hostkey.UnresolvedStatus{Reason: "env A2 is unset"}
	p := b.w.pool(b.hostA, b.a1, b.a2)
	p.Spec.AccountIDs = []string{b.acctA.Meta.ID}
	pol := b.w.policy("pool-a-only", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	h := b.w.start(t)

	is := h.issues(KindPool, p.Meta.ID)
	for field, want := range map[string]string{
		"spec.credentialIds[0]": "key " + b.a1.Meta.Name + " is disabled",
		"spec.credentialIds[1]": "key " + b.a2.Meta.Name + " has no value",
		"spec.accountIds[0]":    "account " + b.acctA.Meta.Name + " has no usable key",
	} {
		i := issueAt(is, IssuePoolMemberUnusable, field)
		if i == nil || !strings.HasPrefix(i.Message, want) || i.Severity != engine.SeverityWarning {
			t.Errorf("%s: %+v, want a warning starting %q", field, i, want)
		}
	}
	if i := issueAt(is, IssuePoolMemberUnusable, "spec.credentialIds[0]"); i != nil && i.Target != (engine.Ref{Kind: KindCredential, ID: b.a1.Meta.ID}) {
		t.Errorf("target = %+v, want the key", i.Target)
	}
	if i := issueAt(h.issues(KindPolicy, pol.Meta.ID), IssuePoolsServeNothing, "spec.poolIds"); i == nil || i.Severity != engine.SeverityError {
		t.Errorf("policy issues = %+v, want %s", h.issues(KindPolicy, pol.Meta.ID), IssuePoolsServeNothing)
	}

	on := clone(b.a1)
	on.Spec.Enabled = nil
	apply(h, on)
	is = h.issues(KindPool, p.Meta.ID)
	if issueAt(is, IssuePoolMemberUnusable, "spec.credentialIds[0]") != nil || issueAt(is, IssuePoolMemberUnusable, "spec.accountIds[0]") != nil {
		t.Errorf("enabled key still reported: %+v", is)
	}
	if hasIssue(h.issues(KindPolicy, pol.Meta.ID), IssuePoolsServeNothing) {
		t.Error("policy still reports its pools serve nothing")
	}
}

// Personal accounts turned off reach the pool through each kind of member: an account the user owns, a key in it, and a share of it.
func TestPoolMemberUnusable_PersonalAccountOff(t *testing.T) {
	s := newSharing()
	share := s.w.share(s.real, account.Receiver{Kind: account.ReceiverTeam, ID: s.team.Meta.ID}, meta.Owner{Kind: meta.OwnerTeam, ID: s.team.Meta.ID})
	p := s.w.pool(s.host, s.key)
	p.Spec.AccountIDs = []string{s.real.Meta.ID, share.Meta.ID}
	pol := s.w.policy("alice-pool", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	h := s.w.start(t)
	if is := h.issues(KindPool, p.Meta.ID); len(is) > 0 {
		t.Fatalf("issues before: %+v", is)
	}

	apply(h, withPersonalAccount(s.alice, settings.PersonalAccountsOff))
	is := h.issues(KindPool, p.Meta.ID)
	for _, field := range []string{"spec.accountIds[0]", "spec.accountIds[1]", "spec.credentialIds[0]"} {
		if i := issueAt(is, IssuePoolMemberUnusable, field); i == nil || !strings.Contains(i.Message, s.alice.Username+", whose personal accounts are turned off") {
			t.Errorf("%s: %+v, want it to name alice's switch", field, i)
		}
	}
	if !hasIssue(h.issues(KindPolicy, pol.Meta.ID), IssuePoolsServeNothing) {
		t.Errorf("policy issues = %+v, want %s", h.issues(KindPolicy, pol.Meta.ID), IssuePoolsServeNothing)
	}
}

// A keyless host's pool serves with no key, so a policy linking it is served; listing accounts only there names spec.accountIds.
func TestPoolsServeNothing_KeylessPoolServes(t *testing.T) {
	b := newBasic()
	b.hostB.Spec.NoAuth = true
	p := &pool.Pool{Meta: md("keyless-"+seq(), system), Spec: pool.Spec{HostID: b.hostB.Meta.ID, AccountIDs: []string{b.acctB.Meta.ID}}}
	b.w.pools = append(b.w.pools, p)
	pol := b.w.policy("keyless", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	h := b.w.start(t)
	if hasIssue(h.issues(KindPolicy, pol.Meta.ID), IssuePoolsServeNothing) {
		t.Errorf("a keyless pool counts as serving nothing: %+v", h.issues(KindPolicy, pol.Meta.ID))
	}
	if issueAt(h.issues(KindPool, p.Meta.ID), IssueCredentialsIgnored, "spec.accountIds") == nil {
		t.Errorf("pool issues = %+v, want %s at spec.accountIds", h.issues(KindPool, p.Meta.ID), IssueCredentialsIgnored)
	}
}

// A disabled host is reported on the pools and keys at it, and on the host while they point at it.
func TestHostDisabled_PoolKeyAndHost(t *testing.T) {
	b := newBasic()
	p := b.w.pool(b.hostA, b.a1)
	pol := b.w.policy("pool-a", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	b.hostA.Spec.Enabled = boolp(false)
	h := b.w.start(t)

	if i := issueAt(h.issues(KindPool, p.Meta.ID), IssueHostDisabled, "spec.hostId"); i == nil || i.Severity != engine.SeverityError || i.Target.ID != b.hostA.Meta.ID {
		t.Errorf("pool issues = %+v, want %s error", h.issues(KindPool, p.Meta.ID), IssueHostDisabled)
	}
	if i := issueAt(h.issues(KindCredential, b.a1.Meta.ID), IssueHostDisabled, "spec.hostId"); i == nil || i.Severity != engine.SeverityWarning {
		t.Errorf("key issues = %+v, want %s warning", h.issues(KindCredential, b.a1.Meta.ID), IssueHostDisabled)
	}
	if !hasIssue(h.issues(KindPolicy, pol.Meta.ID), IssuePoolsServeNothing) {
		t.Errorf("policy issues = %+v, want %s", h.issues(KindPolicy, pol.Meta.ID), IssuePoolsServeNothing)
	}
	is := h.issues(KindHost, b.hostA.Meta.ID)
	if len(is) != 1 || is[0].Code != IssueHostDisabledInUse || !strings.Contains(is[0].Message, "1 pool and 2 enabled keys") {
		t.Errorf("host issues = %+v, want %s naming 1 pool and 2 keys", is, IssueHostDisabledInUse)
	}

	del(h, KindPool, p.Meta.ID)
	for _, k := range []*hostkey.HostKey{b.a1, b.a2} {
		del(h, KindCredential, k.Meta.ID)
	}
	if is := h.issues(KindHost, b.hostA.Meta.ID); len(is) > 0 {
		t.Errorf("host issues with nothing pointing at it = %+v", is)
	}
	if is := h.issues(KindHost, b.hostB.Meta.ID); len(is) > 0 {
		t.Errorf("enabled host issues = %+v", is)
	}
}
