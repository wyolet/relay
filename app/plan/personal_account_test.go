package plan

import (
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
)

func groupSaying(id, value string, enabled bool) *group.Group {
	return &group.Group{Meta: meta.Metadata{ID: id, Owner: system}, Spec: group.Spec{PersonalAccount: value, Enabled: &enabled}}
}

// The user's own value wins; then an enabled group's, off over on; then the deployment's setting, on when unset.
func TestEffectivePersonalAccount(t *testing.T) {
	off := &settings.Accounts{PersonalAccounts: settings.PersonalAccountsOff}
	for _, tc := range []struct {
		name    string
		user    string
		groups  []*group.Group
		setting *settings.Accounts
		on      bool
		source  PersonalAccountSource
		groupID string
	}{
		{name: "nothing set", on: true, source: PersonalAccountFromSetting},
		{name: "setting off", setting: off, on: false, source: PersonalAccountFromSetting},
		{name: "user on beats setting and groups", user: "on", setting: off, groups: []*group.Group{groupSaying("g1", "off", true)}, on: true, source: PersonalAccountFromUser},
		{name: "user off", user: "off", on: false, source: PersonalAccountFromUser},
		{name: "off wins between groups", groups: []*group.Group{groupSaying("g1", "on", true), groupSaying("g2", "off", true)}, on: false, source: PersonalAccountFromGroup, groupID: "g2"},
		{name: "group on beats setting off", setting: off, groups: []*group.Group{groupSaying("g1", "on", true)}, on: true, source: PersonalAccountFromGroup, groupID: "g1"},
		{name: "a disabled group says nothing", groups: []*group.Group{groupSaying("g1", "off", false)}, on: true, source: PersonalAccountFromSetting},
		{name: "a user's group says nothing", groups: []*group.Group{func() *group.Group {
			g := groupSaying("g1", "off", true)
			g.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}
			return g
		}()}, on: true, source: PersonalAccountFromSetting},
		{name: "a group without a value says nothing", setting: off, groups: []*group.Group{groupSaying("g1", "", true)}, on: false, source: PersonalAccountFromSetting},
	} {
		on, source, groupID := EffectivePersonalAccount(tc.user, tc.groups, tc.setting)
		if on != tc.on || source != tc.source || groupID != tc.groupID {
			t.Errorf("%s: on %v from %s %q, want on %v from %s %q", tc.name, on, source, groupID, tc.on, tc.source, tc.groupID)
		}
	}
}

func withPersonalAccount(u *user.User, value string) *user.User {
	c := clone(u)
	c.PersonalAccount = value
	return c
}

// Turning a user's personal account off closes their personal lane and the provider account they own, which stops serving its share; their member lane is untouched, and turning it on restores everything.
func TestPersonalAccountOff_ClosesPersonalLaneAndOwnedAccounts(t *testing.T) {
	s := newSharing()
	bob := s.w.user()
	shared := s.shareWithUser(bob)
	bobKey := s.w.keyIn(bob, shared)
	personalKey := s.w.userKey(s.alice)
	providerLaneKey := s.w.keyIn(s.alice, s.real)
	memberKey := s.w.keyIn(s.alice, s.w.member(s.alice, meta.Owner{Kind: meta.OwnerTeam, ID: s.team.Meta.ID}))
	h := s.w.start(t)
	for _, k := range []string{"personal", "provider lane", "share"} {
		var r *Route
		switch k {
		case "personal":
			r = h.route(personalKey, "m1")
		case "provider lane":
			r = h.route(providerLaneKey, "m1")
		case "share":
			r = h.route(bobKey, "m1")
		}
		if r.Refused != nil {
			t.Fatalf("%s before: %v", k, r.Refused)
		}
	}

	apply(h, withPersonalAccount(s.alice, settings.PersonalAccountsOff))
	for name, k := range map[string]*KeyPlan{"personal": h.keyPlan(personalKey), "provider lane": h.keyPlan(providerLaneKey)} {
		if r := k.Caller.Refused; r == nil || r.Code != CodePersonalAccountOff {
			t.Errorf("%s key: refused %+v, want %s", name, r, CodePersonalAccountOff)
		}
		if is := h.issues(KindKey, k.Key.Meta.ID); !hasIssue(is, IssuePersonalAccountOff) {
			t.Errorf("%s key issues = %+v, want %s", name, is, IssuePersonalAccountOff)
		}
	}
	if is := h.issues(KindAccount, s.real.Meta.ID); !hasIssue(is, IssuePersonalAccountOff) {
		t.Errorf("issues on alice's provider account = %+v, want %s", is, IssuePersonalAccountOff)
	}
	if is := h.issues(KindCredential, s.key.Meta.ID); hasIssue(is, IssueRefMissing) {
		t.Errorf("the credential reports its account missing: %+v", is)
	}
	if r := h.route(bobKey, "m1"); r.Refused == nil || r.Refused.Code != CodeNoCredentials {
		t.Errorf("share after: %+v, want no_credentials", r.Refused)
	}
	if is := h.issues(KindAccount, shared.Meta.ID); !hasIssue(is, IssuePersonalAccountOff) || hasIssue(is, IssueSharedAccountMissing) {
		t.Errorf("issues on the share = %+v, want %s alone", is, IssuePersonalAccountOff)
	}
	if is := h.issues(KindAccount, s.w.personal(s.alice).Meta.ID); !hasIssue(is, IssuePersonalAccountOff) {
		t.Errorf("issues on alice's personal account = %+v, want %s", is, IssuePersonalAccountOff)
	}
	if r := h.keyPlan(memberKey).Caller.Refused; r != nil {
		t.Errorf("member lane refused: %v", r)
	}

	apply(h, withPersonalAccount(s.alice, settings.PersonalAccountsOn))
	if r := h.route(personalKey, "m1"); r.Refused != nil {
		t.Errorf("personal after turning it on: %v", r.Refused)
	}
	if r := h.route(bobKey, "m1"); r.Refused != nil {
		t.Errorf("share after turning it on: %v", r.Refused)
	}
	if is := h.issues(KindKey, personalKey.Meta.ID); hasIssue(is, IssuePersonalAccountOff) {
		t.Errorf("the key still reports %s", IssuePersonalAccountOff)
	}
	if is := h.issues(KindAccount, s.w.personal(s.alice).Meta.ID); hasIssue(is, IssuePersonalAccountOff) {
		t.Errorf("alice's personal account still reports %s", IssuePersonalAccountOff)
	}
}

// The deployment setting turns every personal account off; a group can turn it on again for its members, and a user's own value beats both.
func TestPersonalAccountOff_SettingAndGroups(t *testing.T) {
	s := newSharing()
	carol, dave := s.w.user(), s.w.user()
	s.w.setting = &settings.Accounts{PersonalAccounts: settings.PersonalAccountsOff}
	on := true
	g := &group.Group{Meta: md("pilots-"+seq(), system), Spec: group.Spec{MemberIDs: []string{carol.ID}, PersonalAccount: settings.PersonalAccountsOn, Enabled: &on}}
	s.w.groups = append(s.w.groups, g)
	aliceKey, carolKey, daveKey := s.w.userKey(s.alice), s.w.userKey(carol), s.w.userKey(dave)
	h := s.w.start(t)

	refused := func(k *KeyPlan) bool {
		return k.Caller.Refused != nil && k.Caller.Refused.Code == CodePersonalAccountOff
	}
	if !refused(h.keyPlan(aliceKey)) || !refused(h.keyPlan(daveKey)) || refused(h.keyPlan(carolKey)) {
		t.Fatalf("setting off with carol's group on: alice %v carol %v dave %v", refused(h.keyPlan(aliceKey)), refused(h.keyPlan(carolKey)), refused(h.keyPlan(daveKey)))
	}
	if p := PersonalAccountOf(h.e.View(), carol.ID); p == nil || !p.On || p.Source != PersonalAccountFromGroup || p.GroupID != g.Meta.ID {
		t.Errorf("carol's state = %+v, want on from her group", p)
	}

	apply(h, withPersonalAccount(dave, settings.PersonalAccountsOn))
	if refused(h.keyPlan(daveKey)) {
		t.Error("dave's own on did not beat the setting")
	}
	apply(h, &settings.Accounts{PersonalAccounts: settings.PersonalAccountsOn})
	if refused(h.keyPlan(aliceKey)) {
		t.Error("turning the setting on left alice's personal account closed")
	}
	disabled := false
	g2 := clone(g)
	g2.Spec.PersonalAccount, g2.Spec.Enabled = settings.PersonalAccountsOff, &disabled
	apply(h, g2)
	if refused(h.keyPlan(carolKey)) {
		t.Error("a disabled group turned carol's personal account off")
	}
	g3 := clone(g2)
	g3.Spec.Enabled = &on
	apply(h, g3)
	if !refused(h.keyPlan(carolKey)) {
		t.Error("an enabled group saying off left carol's personal account on")
	}
}
