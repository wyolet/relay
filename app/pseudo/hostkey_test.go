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

// Ids of the rows a host key body names or its expansion reads.
const (
	hkHostID      = "01a20b40-0000-7000-8000-000000000101"
	hkOtherHostID = "01a20b40-0000-7000-8000-000000000102"
	workID        = "01a20b40-0000-7000-8000-000000000103"
	otherID       = "01a20b40-0000-7000-8000-000000000104"
	bobsID        = "01a20b40-0000-7000-8000-000000000105"
	workShareID   = "01a20b40-0000-7000-8000-000000000106"
	freeID        = "01a20b40-0000-7000-8000-000000000107"
	paidID        = "01a20b40-0000-7000-8000-000000000108"
	twoPolicyID   = "01a20b40-0000-7000-8000-000000000109"
	heldID        = "01a20b40-0000-7000-8000-00000000010a"
	twoID         = "01a20b40-0000-7000-8000-00000000010b"
	bobID         = "01a20b40-0000-7000-8000-00000000010c"
	keyOwnerID    = "01a20b40-0000-7000-8000-00000000010d"
	farID         = "01a20b40-0000-7000-8000-00000000010e"
	elsewhereID   = "01a20b40-0000-7000-8000-00000000010f"
)

var (
	keyOwner = meta.Owner{Kind: meta.OwnerUser, ID: keyOwnerID}
	hostA    = meta.Owner{Kind: meta.OwnerHost, ID: hkHostID}
	two      = meta.Owner{Kind: meta.OwnerTeam, ID: twoID}
	bob      = meta.Owner{Kind: meta.OwnerUser, ID: bobID}
)

// hostKeyWorld is what a host key expansion reads: the owner's work account at the host, holding org-a and bound to the free tier, the owner's other account there holding org-b, bob's account there, a share of the work account with team two, the host's free and paid tiers and a policy of the team.
type hostKeyWorld struct {
	work, other, bobs, share *account.Account
	free, paid, teamPolicy   *policy.Policy
	held                     *policybinding.PolicyBinding
}

func newHostKeyWorld() *hostKeyWorld {
	w := &hostKeyWorld{
		work:       &account.Account{Meta: meta.Metadata{ID: workID, Name: "work", Owner: keyOwner}, Spec: account.Spec{HostID: hkHostID, OrgID: "org-a"}},
		other:      &account.Account{Meta: meta.Metadata{ID: otherID, Name: "other", Owner: keyOwner}, Spec: account.Spec{HostID: hkHostID, OrgID: "org-b"}},
		bobs:       &account.Account{Meta: meta.Metadata{ID: bobsID, Name: "bobs", Owner: bob}, Spec: account.Spec{HostID: hkHostID}},
		free:       &policy.Policy{Meta: meta.Metadata{ID: freeID, Name: "alpha-free", Owner: hostA}},
		paid:       &policy.Policy{Meta: meta.Metadata{ID: paidID, Name: "alpha-paid", Owner: hostA}},
		teamPolicy: &policy.Policy{Meta: meta.Metadata{ID: twoPolicyID, Name: "two-policy", Owner: two}},
	}
	w.share = &account.Account{Meta: meta.Metadata{ID: workShareID, Name: "work-for-two", Owner: two}, Spec: account.Spec{HostID: hkHostID, SourceAccountID: workID}}
	w.held = &policybinding.PolicyBinding{Meta: meta.Metadata{ID: heldID, Name: "alpha-free-work", Owner: keyOwner}, Spec: policybinding.Spec{PolicyID: freeID, AccountIDs: []string{workID}}}
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
	if spec.HostID == "" {
		spec.HostID = hkHostID
	}
	return HostKeyForm{Metadata: HostKeyFormMetadata{DisplayName: "Work key", Labels: map[string]string{"env": "prod"}, Owner: keyOwner}, Spec: spec}
}

func storedKeyRow(accountID string) Row {
	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: "hk", Name: "work-key-2", DisplayName: "Work key", Labels: map[string]string{"env": "prod"}, Owner: keyOwner},
		Spec: hostkey.Spec{HostID: hkHostID, AccountID: accountID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}, Value: "sk-test", PricingStrategy: hostkey.StrategySub},
	}
	return Row{Kind: "host-key", Pointer: "/metadata", Meta: &k.Meta, Value: k}
}

func TestExpandHostKey(t *testing.T) {
	w := newHostKeyWorld()
	toTwo := account.Receiver{Kind: account.ReceiverTeam, ID: twoID}
	toBob := account.Receiver{Kind: account.ReceiverUser, ID: bobID}
	toOwner := account.Receiver{Kind: account.ReceiverUser, ID: keyOwnerID}

	newAcct := &account.Account{Meta: meta.Metadata{ID: "id-1", Name: "work", DisplayName: "Work", Owner: keyOwner}, Spec: account.Spec{HostID: hkHostID, OrgID: "org-c"}}
	newBinding := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: "id-2", Name: "alpha-paid-work", Owner: keyOwner}, Spec: policybinding.Spec{PolicyID: paidID, AccountIDs: []string{"id-1"}}}
	bobsShareOfNew := account.Share(newAcct, toBob, bob, "bob")
	bobsShareOfNew.Meta.ID = "id-3"
	bobsShareOfWork := account.Share(w.work, toBob, bob, "bob")
	bobsShareOfWork.Meta.ID = "id-1"
	envKey := &hostkey.HostKey{
		Meta: meta.Metadata{ID: "hk", Name: "work-key-2", DisplayName: "Work key", Labels: map[string]string{"env": "prod"}, Owner: keyOwner},
		Spec: hostkey.Spec{HostID: hkHostID, AccountID: workID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "WORK_KEY"}},
	}
	keyless := &account.Account{Meta: meta.Metadata{ID: "id-1", Name: "account", Owner: keyOwner}, Spec: account.Spec{HostID: hkHostID}}

	for _, tc := range []struct {
		name string
		spec HostKeyFormSpec
		edit func(*HostKeyReads)
		want []Row
	}{
		{
			name: "a new account taking the detected organization, its tier, the key, and a share with each receiver that neither holds one nor reaches the account",
			spec: HostKeyFormSpec{Value: "sk-test", PricingStrategy: hostkey.StrategySub, Account: &HostKeyFormAccount{DisplayName: "Work"}, TierPolicyID: paidID, ShareWith: []account.Receiver{toOwner, toBob}},
			edit: func(in *HostKeyReads) {
				in.Detected, in.Tier = &Organization{ID: "org-c", Name: "C"}, w.paid
				in.Receivers = []ShareReceiver{{Owner: keyOwner, Name: "alice", Reaches: true}, {Owner: bob, Name: "bob"}}
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
			spec: HostKeyFormSpec{ValueFrom: &hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "WORK_KEY"}, AccountID: workID, TierPolicyID: paidID, ShareWith: []account.Receiver{toTwo, toBob}},
			edit: func(in *HostKeyReads) {
				in.Account, in.Holding, in.Tier, in.Detected = w.work, w.held, w.paid, &Organization{ID: "org-a"}
				in.Receivers = []ShareReceiver{{Owner: two, Name: "two", Share: w.share}, {Owner: bob, Name: "bob"}}
			},
			want: []Row{
				func() Row {
					rb := &AccountRebinding{Account: w.work, PolicyID: paidID}
					return Row{Kind: "policy-binding", Pointer: "/spec/tierPolicyId", Meta: &rb.Binding.Meta, Value: rb}
				}(),
				{Kind: "host-key", Pointer: "/metadata", Meta: &envKey.Meta, Value: envKey},
				{Kind: "account", Pointer: "/spec/shareWith/1", Meta: &bobsShareOfWork.Meta, Value: bobsShareOfWork},
			},
		},
		{
			name: "an existing account already bound to the tier, no organization named",
			spec: HostKeyFormSpec{Value: "sk-test", PricingStrategy: hostkey.StrategySub, AccountID: workID, TierPolicyID: freeID},
			edit: func(in *HostKeyReads) { in.Account, in.Holding, in.Tier = w.work, w.held, w.free },
			want: []Row{storedKeyRow(workID)},
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
	team := account.Receiver{Kind: account.ReceiverTeam, ID: twoID}
	for _, tc := range []struct {
		name string
		body HostKeyForm
		edit func(*HostKeyReads)
		want error
	}{
		{"a host by slug", hostKeyBody(HostKeyFormSpec{HostID: "alpha", Value: "sk-test", Account: newAcct}), nil,
			&BodyError{Pointer: "/spec/hostId", Message: `host "alpha" is not an id; name the host by its metadata.id`}},
		{"no host", HostKeyForm{Metadata: HostKeyFormMetadata{Owner: keyOwner}, Spec: HostKeyFormSpec{Value: "sk-test", Account: newAcct}}, nil,
			&BodyError{Pointer: "/spec/hostId", Message: `host "" is not an id; name the host by its metadata.id`}},
		{"an account by slug", hostKeyBody(HostKeyFormSpec{Value: "sk-test", AccountID: "work"}), nil,
			&BodyError{Pointer: "/spec/accountId", Message: `account "work" is not an id; name the account by its metadata.id`}},
		{"a tier by slug", hostKeyBody(HostKeyFormSpec{Value: "sk-test", Account: newAcct, TierPolicyID: "alpha-free"}), nil,
			&BodyError{Pointer: "/spec/tierPolicyId", Message: `policy "alpha-free" is not an id; name the policy by its metadata.id`}},
		{"a receiver by slug", hostKeyBody(HostKeyFormSpec{Value: "sk-test", Account: newAcct, ShareWith: []account.Receiver{team, {Kind: account.ReceiverTeam, ID: "platform"}}}), nil,
			&BodyError{Pointer: "/spec/shareWith/1/id", Message: `receiver "platform" is not an id; name the receiver by its metadata.id`}},
		{"an owner by name", func() HostKeyForm {
			b := hostKeyBody(HostKeyFormSpec{Value: "sk-test", Account: newAcct})
			b.Metadata.Owner.ID = "alice"
			return b
		}(), nil, &BodyError{Pointer: "/metadata/owner/id", Message: `owner "alice" is not an id; name the owner by its metadata.id`}},
		{"neither accountId nor account", hostKeyBody(HostKeyFormSpec{Value: "sk-test"}), nil,
			&BodyError{Pointer: "/spec", Message: "exactly one of spec.accountId and spec.account is required"}},
		{"both accountId and account", hostKeyBody(HostKeyFormSpec{Value: "sk-test", AccountID: workID, Account: newAcct}), nil,
			&BodyError{Pointer: "/spec", Message: "exactly one of spec.accountId and spec.account is required"}},
		{"both value and valueFrom", hostKeyBody(HostKeyFormSpec{Value: "sk-test", ValueFrom: env, Account: newAcct}), nil,
			&BodyError{Pointer: "/spec", Message: "spec.value and spec.valueFrom exclude each other; send one"}},
		{"no value for a host that takes a key", hostKeyBody(HostKeyFormSpec{Account: newAcct}), nil,
			&BodyError{Pointer: "/spec", Message: `host "alpha" takes a key: exactly one of spec.value and spec.valueFrom is required`}},
		{"a value for a host that takes none", hostKeyBody(HostKeyFormSpec{Value: "sk-test", Account: newAcct}), func(in *HostKeyReads) { in.Keyless = true },
			&BodyError{Pointer: "/spec/value", Message: `host "alpha" takes no key; leave spec.value out`}},
		{"valueFrom for a host that takes none", hostKeyBody(HostKeyFormSpec{ValueFrom: env, Account: newAcct}), func(in *HostKeyReads) { in.Keyless = true },
			&BodyError{Pointer: "/spec/valueFrom", Message: `host "alpha" takes no key; leave spec.valueFrom out`}},
		{"a receiver twice", hostKeyBody(HostKeyFormSpec{Value: "sk-test", Account: newAcct, ShareWith: []account.Receiver{team, team}}), nil,
			&BodyError{Pointer: "/spec/shareWith/1", Message: `receiver team "` + twoID + `" is listed twice`}},
		{"another host's tier", hostKeyBody(HostKeyFormSpec{Value: "sk-test", Account: newAcct, TierPolicyID: elsewhereID}),
			func(in *HostKeyReads) {
				in.Tier = &policy.Policy{Meta: meta.Metadata{ID: elsewhereID, Name: "beta-free", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hkOtherHostID}}}
			},
			&BodyError{Pointer: "/spec/tierPolicyId", Message: `policy "beta-free" is not a tier preset of host "alpha"; GET /hosts/{ref}/policies lists them`}},
		{"a policy that is no tier", hostKeyBody(HostKeyFormSpec{Value: "sk-test", Account: newAcct, TierPolicyID: twoPolicyID}), func(in *HostKeyReads) { in.Tier = w.teamPolicy },
			&BodyError{Pointer: "/spec/tierPolicyId", Message: `policy "two-policy" is not a tier preset of host "alpha"; GET /hosts/{ref}/policies lists them`}},
		{"an account of another owner", hostKeyBody(HostKeyFormSpec{Value: "sk-test", AccountID: bobsID}), func(in *HostKeyReads) { in.Account = w.bobs },
			&BodyError{Pointer: "/spec/accountId", Message: `account "bobs" is not a provider account of the key's owner at host "alpha"`}},
		{"an account at another host", hostKeyBody(HostKeyFormSpec{Value: "sk-test", AccountID: farID}),
			func(in *HostKeyReads) {
				in.Account = &account.Account{Meta: meta.Metadata{ID: farID, Name: "far", Owner: keyOwner}, Spec: account.Spec{HostID: hkOtherHostID}}
			},
			&BodyError{Pointer: "/spec/accountId", Message: `account "far" is not a provider account of the key's owner at host "alpha"`}},
		{"a share", hostKeyBody(HostKeyFormSpec{Value: "sk-test", AccountID: workShareID}),
			func(in *HostKeyReads) {
				s := *w.share
				s.Meta.Owner = keyOwner
				in.Account = &s
			},
			&BodyError{Pointer: "/spec/accountId", Message: `account "work-for-two" is not a provider account of the key's owner at host "alpha"`}},
		{"an organization another account of the owner holds", hostKeyBody(HostKeyFormSpec{Value: "sk-test", AccountID: workID}),
			func(in *HostKeyReads) { in.Account, in.Detected = w.work, &Organization{ID: "org-b"} },
			&OrganizationError{Pointer: "/spec/accountId", Holder: w.other, Message: `the key belongs to organization "org-b", which account "other" of the owner holds; send accountId ` + otherID + ` to add the key there`}},
		{"a new account for an organization the owner holds", hostKeyBody(HostKeyFormSpec{Value: "sk-test", Account: newAcct}),
			func(in *HostKeyReads) { in.Detected = &Organization{ID: "org-a"} },
			&OrganizationError{Pointer: "/spec/account", Holder: w.work, Message: `the key belongs to organization "org-a", which account "work" of the owner holds; send accountId ` + workID + ` to add the key there`}},
		{"an account that holds another organization", hostKeyBody(HostKeyFormSpec{Value: "sk-test", AccountID: workID}),
			func(in *HostKeyReads) { in.Account, in.Detected = w.work, &Organization{ID: "org-z"} },
			&OrganizationError{Pointer: "/spec/accountId", Message: `the key belongs to organization "org-z", but account "work" is organization "org-a"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := w.reads()
			if tc.edit != nil {
				tc.edit(&in)
			}
			rows, err := ExpandHostKey(tc.body, in)
			if diff := cmp.Diff(tc.want, err); diff != "" || rows != nil {
				t.Errorf("rows %v, error (-want +got):\n%s", rows, diff)
			}
		})
	}
}
