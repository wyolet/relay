package pipeline

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/plan"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// A request through a shared account records the real account, whose bill it is, and the share it went through.
func TestStampAttemptRecordsTheShare(t *testing.T) {
	hop := buildHop(keypool.KeySelectionPrioritized, nil, testSlot{key: testKey("k"), acc: accountPlan("acct-real")})
	slot := hop.Slots[0]
	slot.Via = plan.Rung{Kind: limits.RungAccount, SubjectID: "account:acct-share", Account: &account.Account{Meta: meta.Metadata{ID: "acct-share"}}}
	lc := lifecycle.NewContext("req", "pipeline", time.Now())
	stampAttempt(lc, &attempt{hop: &hop, slot: slot}, 1)
	if lc.AccountID != "acct-real" || lc.ViaAccountID != "acct-share" {
		t.Fatalf("account %q via %q, want acct-real via acct-share", lc.AccountID, lc.ViaAccountID)
	}

	slot.Via = plan.Rung{}
	stampAttempt(lc, &attempt{hop: &hop, slot: slot}, 2)
	if lc.ViaAccountID != "" {
		t.Fatalf("a direct attempt after a shared one kept via %q", lc.ViaAccountID)
	}
}

// A share's own limit holds only for its holders, so its refusal leaves the real account's keys to everyone else; the real account's tier still cools them.
func TestShareRefusalCoolsNothing(t *testing.T) {
	window := perMinute("rl-req", appratelimit.MeterRequests, appratelimit.StrategyFixedWindow, 1)
	for _, tc := range []struct {
		name  string
		c     limits.Counter
		cools bool
	}{
		{"the share's limit", accountCounters("acct-share", window)[0], false},
		{"the real account's tier", accountCounters("acct-real", window)[0], true},
	} {
		slot := &plan.KeySlot{Account: accountPlan("acct-real"), Via: plan.Rung{SubjectID: "account:acct-share"}}
		coolAccount(slot, &limits.Refused{Counter: &tc.c, RetryAfter: time.Minute})
		if got := slot.Account.Cooldown.Cooled(time.Now().UnixMilli(), "m"); got != tc.cools {
			t.Errorf("%s: cooled = %v, want %v", tc.name, got, tc.cools)
		}
	}
}
