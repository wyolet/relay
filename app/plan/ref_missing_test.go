package plan

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/engine"
)

// ref_missing names the missing row by its kind in words, never by id; the id stays in the issue's target.
func TestRefMissing_MessagesInWords(t *testing.T) {
	b := newBasic()
	w := b.w
	gone := meta.NewID
	pol := w.policy("dangling", policy.Spec{
		RateLimits: []policy.RateLimitLink{{RateLimitID: gone()}},
		Budgets:    []policy.BudgetLink{{BudgetID: gone()}},
		PoolIDs:    []string{gone()},
	})
	p := w.pool(b.hostA, b.a1)
	p.Spec.CredentialIDs = append(p.Spec.CredentialIDs, gone())
	p.Spec.AccountIDs = []string{gone()}
	orphanPool := &pool.Pool{Meta: md("orphan-"+seq(), system), Spec: pool.Spec{HostID: gone()}}
	w.pools = append(w.pools, orphanPool)
	cred := &hostkey.HostKey{Meta: md("cred-"+seq(), system), Spec: hostkey.Spec{HostID: gone(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}}}
	rotate(cred, "sk-orphan")
	w.creds = append(w.creds, cred)
	k := w.keyFor(key.PrincipalServiceAccount, b.sa.Meta.ID, b.w.saAccount(b.sa))
	k.Spec.AccountID = gone()
	h := w.start(t)

	want := map[issueSite]string{
		{KindPolicy, pol.Meta.ID, "spec.rateLimits[0]"}: "links a rate limit that was deleted; the link is ignored",
		{KindPolicy, pol.Meta.ID, "spec.budgets[0]"}:    "links a budget that was deleted; the link is ignored",
		{KindPolicy, pol.Meta.ID, "spec.poolIds[0]"}:    "links a pool that was deleted; the link is ignored",
		{KindPool, p.Meta.ID, "spec.credentialIds[1]"}:  "links a host key that was deleted; the link is ignored",
		{KindPool, p.Meta.ID, "spec.accountIds[0]"}:     "links an account that was deleted; the link is ignored",
		{KindPool, orphanPool.Meta.ID, "spec.hostId"}:   "names a host that was deleted; the pool cannot serve",
		{KindCredential, cred.Meta.ID, "spec.hostId"}:   "names a host that was deleted; the host key cannot serve",
		{KindKey, k.Meta.ID, "spec.accountId"}:          "names an account that was deleted; the relay key cannot serve",
	}
	for at, msg := range want {
		i := issueAt(h.issues(at.kind, at.id), IssueRefMissing, at.field)
		if i == nil || i.Message != msg || i.Target.ID == "" {
			t.Errorf("%s %s: %+v, want %q with the id in its target", at.kind, at.field, i, msg)
		}
	}
	for _, i := range h.e.View().AllIssues() {
		if i.Code == IssueRefMissing && (strings.Contains(i.Message, i.Target.ID) && i.Target.ID != "" || strings.Contains(i.Message, i.Source.ID)) {
			t.Errorf("message carries an id: %+v", i)
		}
	}
}

// issueSite is where one issue sits: the row and the field.
type issueSite struct {
	kind  engine.Kind
	id    string
	field string
}
