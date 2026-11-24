package pseudo

import (
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// hostKeyWorld is what a host key expansion reads: alice's account at host a holding org-a and bound to the free tier, alice's other account there holding org-b, bob's account there, a share of alice's account with team two, the host's free and paid tiers and a policy of the team.
type hostKeyWorld struct {
	work, other, bobs, share *account.Account
	free, paid, teamPolicy   *policy.Policy
	held                     *policybinding.PolicyBinding
}

var (
	hostA = meta.Owner{Kind: meta.OwnerHost, ID: "host-a"}
	two   = meta.Owner{Kind: meta.OwnerTeam, ID: "two"}
)

func newHostKeyWorld() *hostKeyWorld {
	w := &hostKeyWorld{
		work:       &account.Account{Meta: meta.Metadata{ID: "work", Name: "work", Owner: alice}, Spec: account.Spec{HostID: "host-a", OrgID: "org-a"}},
		other:      &account.Account{Meta: meta.Metadata{ID: "other", Name: "other", Owner: alice}, Spec: account.Spec{HostID: "host-a", OrgID: "org-b"}},
		bobs:       &account.Account{Meta: meta.Metadata{ID: "bobs", Name: "bobs", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "bob"}}, Spec: account.Spec{HostID: "host-a"}},
		free:       &policy.Policy{Meta: meta.Metadata{ID: "free", Name: "alpha-free", Owner: hostA}},
		paid:       &policy.Policy{Meta: meta.Metadata{ID: "paid", Name: "alpha-paid", Owner: hostA}},
		teamPolicy: &policy.Policy{Meta: meta.Metadata{ID: "team-policy", Name: "two-policy", Owner: two}},
	}
	w.share = &account.Account{Meta: meta.Metadata{ID: "work-share", Name: "work-for-two", Owner: two}, Spec: account.Spec{HostID: "host-a", SourceAccountID: "work"}}
	w.held = &policybinding.PolicyBinding{Meta: meta.Metadata{ID: "held", Name: "alpha-free-work", Owner: alice}, Spec: policybinding.Spec{PolicyID: "free", AccountIDs: []string{"work"}}}
	return w
}

func (w *hostKeyWorld) reads() HostKeyReads {
	n := 0
	return HostKeyReads{
		OwnerAccounts: []*account.Account{w.work, w.other},
		HostName:      "alpha",
		HostKeyID:     "hk",
		Names:         &Namer{Taken: func(kind, name string) bool { return kind == "host-key" && name == "work-key" }},
		NewID:         func() string { n++; return "id-" + strconv.Itoa(n) },
	}
}

func hostKeyBody(spec HostKeyFormSpec) HostKeyForm {
	spec.HostID = "host-a"
	return HostKeyForm{Metadata: HostKeyFormMetadata{DisplayName: "Work key", Labels: map[string]string{"env": "prod"}, Owner: alice}, Spec: spec}
}

func storedKeyRow(accountID string) Row {
	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: "hk", Name: "work-key-2", DisplayName: "Work key", Labels: map[string]string{"env": "prod"}, Owner: alice},
		Spec: hostkey.Spec{HostID: "host-a", AccountID: accountID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}, Value: "sk-test", PricingStrategy: hostkey.StrategySub},
	}
	return Row{Kind: "host-key", Pointer: "/metadata", Meta: &k.Meta, Value: k}
}

func TestExpandHostKey(t *testing.T) {
	w := newHostKeyWorld()
	toTwo := account.Receiver{Kind: account.ReceiverTeam, ID: "two"}
	toBob := account.Receiver{Kind: account.ReceiverUser, ID: "bob"}
	toAlice := account.Receiver{Kind: account.ReceiverUser, ID: "alice"}
	bob := meta.Owner{Kind: meta.OwnerUser, ID: "bob"}

	newAcct := &account.Account{Meta: meta.Metadata{ID: "id-1", Name: "work", DisplayName: "Work", Owner: alice}, Spec: account.Spec{HostID: "host-a", OrgID: "org-c"}}
	newBinding := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: "id-2", Name: "alpha-paid-work", Owner: alice}, Spec: policybinding.Spec{PolicyID: "paid", AccountIDs: []string{"id-1"}}}
	bobsShareOfNew := account.Share(newAcct, toBob, bob, "bob")
	bobsShareOfNew.Meta.ID = "id-3"
	bobsShareOfWork := account.Share(w.work, toBob, bob, "bob")
	bobsShareOfWork.Meta.ID = "id-1"
	envKey := &hostkey.HostKey{
		Meta: meta.Metadata{ID: "hk", Name: "work-key-2", DisplayName: "Work key", Labels: map[string]string{"env": "prod"}, Owner: alice},
		Spec: hostkey.Spec{HostID: "host-a", AccountID: "work", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "WORK_KEY"}},
	}
	keyless := &account.Account{Meta: meta.Metadata{ID: "id-1", Name: "account", Owner: alice}, Spec: account.Spec{HostID: "host-a"}}

	for _, tc := range []struct {
		name string
		spec HostKeyFormSpec
		edit func(*HostKeyReads)
		want []Row
	}{
		{
			name: "a new account taking the detected organization, its tier, the key, and a share with each receiver that neither holds one nor reaches the account",
			spec: HostKeyFormSpec{Value: "sk-test", PricingStrategy: hostkey.StrategySub, Account: &HostKeyFormAccount{DisplayName: "Work"}, TierPolicyID: "paid", ShareWith: []account.Receiver{toAlice, toBob}},
			edit: func(in *HostKeyReads) {
				in.Detected, in.Tier = &Organization{ID: "org-c", Name: "C"}, w.paid
				in.Receivers = []ShareReceiver{{Owner: alice, Name: "alice", Reaches: true}, {Owner: bob, Name: "bob"}}
				in.Names = &Namer{Taken: func(kind, name string) bool { return kind == "host-key" && name == "work-key" }}
			},
			want: []Row{
				{Kind: "account", Pointer: "/spec/account", Meta: &newAcct.Meta, Value: newAcct},
				{Kind: "policy-binding", Pointer: "/spec/tierPolicyId", Meta: &newBinding.Meta, Value: newBinding},
				storedKeyRow("id-1"),
				{Kind: "account", Pointer: "/spec/shareWith/1", Meta: &bobsShareOfNew.Meta, Value: bobsShareOfNew},
			},
		},
		{
			name: "an existing account with the detected organization, rebound to a tier, a key from the environment, a receiver that holds a share already and one that has none",
			spec: HostKeyFormSpec{ValueFrom: &hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "WORK_KEY"}, AccountID: "work", TierPolicyID: "paid", ShareWith: []account.Receiver{toTwo, toBob}},
			edit: func(in *HostKeyReads) {
				in.Account, in.Holding, in.Tier, in.Detected = w.work, w.held, w.paid, &Organization{ID: "org-a"}
				in.Receivers = []ShareReceiver{{Owner: two, Name: "two", Share: w.share}, {Owner: bob, Name: "bob"}}
			},
			want: []Row{
				func() Row {
					rb := &AccountRebinding{Account: w.work, PolicyID: "paid"}
					return Row{Kind: "policy-binding", Pointer: "/spec/tierPolicyId", Meta: &rb.Binding.Meta, Value: rb}
				}(),
				{Kind: "host-key", Pointer: "/metadata", Meta: &envKey.Meta, Value: envKey},
				{Kind: "account", Pointer: "/spec/shareWith/1", Meta: &bobsShareOfWork.Meta, Value: bobsShareOfWork},
			},
		},
		{
			name: "an existing account already bound to the tier, no organization named",
			spec: HostKeyFormSpec{Value: "sk-test", PricingStrategy: hostkey.StrategySub, AccountID: "work", TierPolicyID: "free"},
			edit: func(in *HostKeyReads) { in.Account, in.Holding, in.Tier = w.work, w.held, w.free },
			want: []Row{storedKeyRow("work")},
		},
		{
			name: "a host that takes no key: the account alone",
			spec: HostKeyFormSpec{Account: &HostKeyFormAccount{}},
			edit: func(in *HostKeyReads) { in.Keyless = true },
			want: []Row{{Kind: "account", Pointer: "/spec/account", Meta: &keyless.Meta, Value: keyless}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := w.reads()
			tc.edit(&in)
			got, err := ExpandHostKey(hostKeyBody(tc.spec), in)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("rows (-want +got):\n%s", diff)
			}
		})
	}
}

func TestExpandHostKey_Refusals(t *testing.T) {
	w := newHostKeyWorld()
	env := &hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "WORK_KEY"}
	newAcct := &HostKeyFormAccount{DisplayName: "Work"}
	team := account.Receiver{Kind: account.ReceiverTeam, ID: "two"}
	for _, tc := range []struct {
		name string
		spec HostKeyFormSpec
		edit func(*HostKeyReads)
		want error
	}{
		{"neither accountId nor account", HostKeyFormSpec{Value: "sk-test"}, nil,
			&BodyError{Pointer: "/spec", Message: "exactly one of spec.accountId and spec.account is required"}},
		{"both accountId and account", HostKeyFormSpec{Value: "sk-test", AccountID: "work", Account: newAcct}, nil,
			&BodyError{Pointer: "/spec", Message: "exactly one of spec.accountId and spec.account is required"}},
		{"both value and valueFrom", HostKeyFormSpec{Value: "sk-test", ValueFrom: env, Account: newAcct}, nil,
			&BodyError{Pointer: "/spec", Message: "spec.value and spec.valueFrom exclude each other; send one"}},
		{"no value for a host that takes a key", HostKeyFormSpec{Account: newAcct}, nil,
			&BodyError{Pointer: "/spec", Message: `host "alpha" takes a key: exactly one of spec.value and spec.valueFrom is required`}},
		{"a value for a host that takes none", HostKeyFormSpec{Value: "sk-test", Account: newAcct}, func(in *HostKeyReads) { in.Keyless = true },
			&BodyError{Pointer: "/spec/value", Message: `host "alpha" takes no key; leave spec.value out`}},
		{"valueFrom for a host that takes none", HostKeyFormSpec{ValueFrom: env, Account: newAcct}, func(in *HostKeyReads) { in.Keyless = true },
			&BodyError{Pointer: "/spec/valueFrom", Message: `host "alpha" takes no key; leave spec.valueFrom out`}},
		{"a receiver twice", HostKeyFormSpec{Value: "sk-test", Account: newAcct, ShareWith: []account.Receiver{team, team}}, nil,
			&BodyError{Pointer: "/spec/shareWith/1", Message: `receiver team "two" is listed twice`}},
		{"another host's tier", HostKeyFormSpec{Value: "sk-test", Account: newAcct, TierPolicyID: "elsewhere"},
			func(in *HostKeyReads) {
				in.Tier = &policy.Policy{Meta: meta.Metadata{ID: "elsewhere", Name: "beta-free", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "host-b"}}}
			},
			&BodyError{Pointer: "/spec/tierPolicyId", Message: `policy "beta-free" is not a tier preset of host "alpha"; GET /hosts/{ref}/policies lists them`}},
		{"a policy that is no tier", HostKeyFormSpec{Value: "sk-test", Account: newAcct, TierPolicyID: "team-policy"}, func(in *HostKeyReads) { in.Tier = w.teamPolicy },
			&BodyError{Pointer: "/spec/tierPolicyId", Message: `policy "two-policy" is not a tier preset of host "alpha"; GET /hosts/{ref}/policies lists them`}},
		{"an account of another owner", HostKeyFormSpec{Value: "sk-test", AccountID: "bobs"}, func(in *HostKeyReads) { in.Account = w.bobs },
			&BodyError{Pointer: "/spec/accountId", Message: `account "bobs" is not a provider account of the key's owner at host "alpha"`}},
		{"an account at another host", HostKeyFormSpec{Value: "sk-test", AccountID: "far"},
			func(in *HostKeyReads) {
				in.Account = &account.Account{Meta: meta.Metadata{ID: "far", Name: "far", Owner: alice}, Spec: account.Spec{HostID: "host-b"}}
			},
			&BodyError{Pointer: "/spec/accountId", Message: `account "far" is not a provider account of the key's owner at host "alpha"`}},
		{"a share", HostKeyFormSpec{Value: "sk-test", AccountID: "work-share"},
			func(in *HostKeyReads) {
				s := *w.share
				s.Meta.Owner = alice
				in.Account = &s
			},
			&BodyError{Pointer: "/spec/accountId", Message: `account "work-for-two" is not a provider account of the key's owner at host "alpha"`}},
		{"an organization another account of the owner holds", HostKeyFormSpec{Value: "sk-test", AccountID: "work"},
			func(in *HostKeyReads) { in.Account, in.Detected = w.work, &Organization{ID: "org-b"} },
			&OrganizationError{Pointer: "/spec/accountId", Holder: w.other, Message: `the key belongs to organization "org-b", which account "other" of the owner holds; send accountId other to add the key there`}},
		{"a new account for an organization the owner holds", HostKeyFormSpec{Value: "sk-test", Account: newAcct},
			func(in *HostKeyReads) { in.Detected = &Organization{ID: "org-a"} },
			&OrganizationError{Pointer: "/spec/account", Holder: w.work, Message: `the key belongs to organization "org-a", which account "work" of the owner holds; send accountId work to add the key there`}},
		{"an account that holds another organization", HostKeyFormSpec{Value: "sk-test", AccountID: "work"},
			func(in *HostKeyReads) { in.Account, in.Detected = w.work, &Organization{ID: "org-z"} },
			&OrganizationError{Pointer: "/spec/accountId", Message: `the key belongs to organization "org-z", but account "work" is organization "org-a"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := w.reads()
			if tc.edit != nil {
				tc.edit(&in)
			}
			rows, err := ExpandHostKey(hostKeyBody(tc.spec), in)
			if diff := cmp.Diff(tc.want, err); diff != "" || rows != nil {
				t.Errorf("rows %v, error (-want +got):\n%s", rows, diff)
			}
		})
	}
}
