package pseudo

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// Ids of the rows a removal names.
const (
	botID      = "01a20b40-0000-7000-8000-0000000000b1"
	botPolicy  = "01a20b40-0000-7000-8000-0000000000b2"
	botPool    = "01a20b40-0000-7000-8000-0000000000b3"
	otherPool  = "01a20b40-0000-7000-8000-0000000000b4"
	otherScope = "01a20b40-0000-7000-8000-0000000000b5"
)

// removalWorld is a service account of proj with its lane bound alone to its labelled policy, which links its labelled pool; two keys; a share it holds, bound with another account by a tier binding.
type removalWorld struct {
	sa             *serviceaccount.ServiceAccount
	lane, share    *account.Account
	keys           []*key.Key
	held, tierBind *policybinding.PolicyBinding
	pol            *policy.Policy
	pl             *pool.Pool
}

func newRemovalWorld() *removalWorld {
	w := &removalWorld{sa: &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: botID, Name: "bot", Owner: proj}}}
	w.lane = account.ForServiceAccount(botID, "bot", "", proj)
	w.lane.Meta.ID = "lane"
	w.share = &account.Account{Meta: meta.Metadata{ID: "share", Name: "share", Owner: proj}, Spec: account.Spec{HostID: "host-a", SourceAccountID: "src", Holder: &account.Holder{Kind: account.HolderServiceAccount, ID: botID}}}
	for _, id := range []string{"k1", "k2"} {
		w.keys = append(w.keys, &key.Key{Meta: meta.Metadata{ID: id, Name: id}, Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: botID}, AccountID: "lane"}})
	}
	labels := map[string]string{AgentLabel: botID}
	w.pol = &policy.Policy{Meta: meta.Metadata{ID: botPolicy, Name: "bot-policy", Owner: proj, Labels: labels}, Spec: policy.Spec{PoolIDs: []string{botPool}}}
	w.pl = &pool.Pool{Meta: meta.Metadata{ID: botPool, Name: "bot-pool", Owner: proj, Labels: labels}}
	w.held = &policybinding.PolicyBinding{Meta: meta.Metadata{ID: "held", Name: "held", Owner: proj}, Spec: policybinding.Spec{PolicyID: botPolicy, AccountIDs: []string{"lane"}}}
	w.tierBind = &policybinding.PolicyBinding{Meta: meta.Metadata{ID: "tier", Name: "tier", Owner: proj}, Spec: policybinding.Spec{PolicyID: "tier-policy", AccountIDs: []string{"share", "other"}}}
	return w
}

func (w *removalWorld) reads() RemovalReads {
	return RemovalReads{
		ServiceAccount: w.sa, Lane: w.lane, Held: w.held, Keys: w.keys, Shares: []*account.Account{w.share},
		Bindings: []*policybinding.PolicyBinding{w.held, w.tierBind}, Policies: []*policy.Policy{w.pol},
		Labelled: []Labelled{
			{Kind: "policy", Meta: &w.pol.Meta, Value: w.pol, Visible: true, InScope: true, Deletable: true},
			{Kind: "pool", Meta: &w.pl.Meta, Value: w.pl, Visible: true, InScope: true, Deletable: true},
		},
	}
}

func removalBody(keep ...string) AgentRemoval {
	return AgentRemoval{Spec: AgentRemovalSpec{ServiceAccountID: botID, Keep: keep}}
}

func removalRow(kind string, m *meta.Metadata, v any) Row {
	return Row{Kind: kind, Pointer: removalPointer, Meta: m, Value: v, Op: OpDelete}
}

// Keys, the lane out of its binding, the share revoked, the service account with its lane, then the labelled policy and pool: the order every ordinary delete accepts. The binding that held only the lane goes; the tier binding holding the share and another account shrinks.
func TestExpandAgentRemoval_Order(t *testing.T) {
	w := newRemovalWorld()
	got, err := ExpandAgentRemoval(removalBody(), w.reads())
	if err != nil {
		t.Fatal(err)
	}
	shrunk := *w.tierBind
	shrunk.Spec.AccountIDs = []string{"other"}
	rb := &AccountRebinding{Account: w.lane, Held: w.held}
	want := RemovalChange{Rows: []Row{
		removalRow("key", &w.keys[0].Meta, w.keys[0]),
		removalRow("key", &w.keys[1].Meta, w.keys[1]),
		{Kind: "policy-binding", Pointer: removalPointer, Meta: &rb.Binding.Meta, Value: rb},
		removalRow("account", &w.share.Meta, &Removal{Row: w.share, Along: []Row{{Kind: "policy-binding", Pointer: removalPointer, Meta: &shrunk.Meta, Value: &shrunk, Op: OpUpdate}}}),
		removalRow("service-account", &w.sa.Meta, &Removal{Row: w.sa, Along: []Row{removalRow("account", &w.lane.Meta, w.lane)}}),
		removalRow("policy", &w.pol.Meta, w.pol),
		removalRow("pool", &w.pl.Meta, w.pl),
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	var deleted []string
	for _, r := range Changed(got.Rows, OpDelete) {
		deleted = append(deleted, r.Kind+"/"+r.Meta.ID)
	}
	wantDeleted := []string{"key/k1", "key/k2", "policy-binding/held", "account/share", "service-account/" + botID, "account/lane", "policy/" + botPolicy, "pool/" + botPool}
	if diff := cmp.Diff(wantDeleted, deleted); diff != "" {
		t.Errorf("deleted (-want +got):\n%s", diff)
	}
	var updated []string
	for _, r := range Changed(got.Rows, OpUpdate) {
		updated = append(updated, r.Kind+"/"+r.Meta.ID)
	}
	if diff := cmp.Diff([]string{"policy-binding/tier"}, updated); diff != "" {
		t.Errorf("updated (-want +got):\n%s", diff)
	}
}

// A labelled row stays, and is listed with why, unless the scope owns it, the caller sees and may delete it, keep leaves it out and nothing left uses it; one owned elsewhere the caller may not see is neither deleted nor listed.
func TestExpandAgentRemoval_Kept(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(w *removalWorld, in *RemovalReads)
		keep []string
		want []KeptRow
		gone []string
	}{
		{name: "nothing keeps them", gone: []string{botPolicy, botPool}},
		{name: "kept by request", keep: []string{botPool}, gone: []string{botPolicy}, want: []KeptRow{{Kind: "pool", Reason: KeptByRequest, Visible: true}}},
		{name: "the policy kept keeps its pool in use", keep: []string{botPolicy}, want: []KeptRow{{Kind: "policy", Reason: KeptByRequest, Visible: true}, {Kind: "pool", Reason: KeptInUse, Visible: true}}},
		{name: "the binding holds another account", edit: func(w *removalWorld, in *RemovalReads) {
			w.held.Spec.AccountIDs = []string{"lane", "sibling"}
		}, want: []KeptRow{{Kind: "policy", Reason: KeptInUse, Visible: true}, {Kind: "pool", Reason: KeptInUse, Visible: true}}},
		{name: "a policy the caller may not see links the pool", edit: func(w *removalWorld, in *RemovalReads) {
			in.Policies = append(in.Policies, &policy.Policy{Meta: meta.Metadata{ID: "hidden", Owner: teamOwner}, Spec: policy.Spec{PoolIDs: []string{botPool}}})
		}, gone: []string{botPolicy}, want: []KeptRow{{Kind: "pool", Reason: KeptInUse, Visible: true}}},
		{name: "not deletable", edit: func(w *removalWorld, in *RemovalReads) { in.Labelled[0].Deletable = false },
			want: []KeptRow{{Kind: "policy", Reason: KeptNotDeletable, Visible: true}, {Kind: "pool", Reason: KeptInUse, Visible: true}}},
		{name: "not visible in the scope", edit: func(w *removalWorld, in *RemovalReads) { in.Labelled[1].Visible = false },
			gone: []string{botPolicy}, want: []KeptRow{{Kind: "pool", Reason: KeptNotVisible}}},
		{name: "owned elsewhere and seen", edit: func(w *removalWorld, in *RemovalReads) { in.Labelled[1].InScope = false },
			gone: []string{botPolicy}, want: []KeptRow{{Kind: "pool", Reason: KeptNotDeletable, Visible: true}}},
		{name: "owned elsewhere and hidden", edit: func(w *removalWorld, in *RemovalReads) {
			p := &pool.Pool{Meta: meta.Metadata{ID: otherPool, Name: "spoof", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: otherScope}, Labels: map[string]string{AgentLabel: botID}}}
			in.Labelled = append(in.Labelled, Labelled{Kind: "pool", Meta: &p.Meta, Value: p})
		}, gone: []string{botPolicy, botPool}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newRemovalWorld()
			in := w.reads()
			if tc.edit != nil {
				tc.edit(w, &in)
			}
			got, err := ExpandAgentRemoval(removalBody(tc.keep...), in)
			if err != nil {
				t.Fatal(err)
			}
			var gone []string
			for _, r := range got.Rows {
				if r.Kind == "policy" || r.Kind == "pool" {
					gone = append(gone, r.Meta.ID)
				}
			}
			for i := range got.Kept {
				got.Kept[i].Meta = nil
			}
			if diff := cmp.Diff(tc.gone, gone); diff != "" {
				t.Errorf("deleted (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.want, got.Kept); diff != "" {
				t.Errorf("kept (-want +got):\n%s", diff)
			}
		})
	}
}

// keep names only labelled rows the answer lists; a hidden row owned elsewhere is not one, so naming it says nothing of it.
func TestExpandAgentRemoval_KeepRefused(t *testing.T) {
	w := newRemovalWorld()
	in := w.reads()
	p := &pool.Pool{Meta: meta.Metadata{ID: otherPool, Owner: meta.Owner{Kind: meta.OwnerTeam, ID: otherScope}, Labels: map[string]string{AgentLabel: botID}}}
	in.Labelled = append(in.Labelled, Labelled{Kind: "pool", Meta: &p.Meta, Value: p})
	for _, tc := range []struct {
		keep []string
		want *BodyError
	}{
		{[]string{botPool, otherPool}, &BodyError{Pointer: "/spec/keep/1", Message: `"` + otherPool + `" is not a policy or pool labelled agent-id for this service account`}},
		{[]string{botPool, botPool}, &BodyError{Pointer: "/spec/keep/1", Message: `keep entry "` + botPool + `" is not an id, or is listed twice`}},
		{[]string{"bot-pool"}, &BodyError{Pointer: "/spec/keep/0", Message: `keep entry "bot-pool" is not an id, or is listed twice`}},
	} {
		_, err := ExpandAgentRemoval(removalBody(tc.keep...), in)
		if diff := cmp.Diff(tc.want, err); diff != "" {
			t.Errorf("keep %v (-want +got):\n%s", tc.keep, diff)
		}
	}
	_, err := ExpandAgentRemoval(AgentRemoval{Spec: AgentRemovalSpec{ServiceAccountID: "bot"}}, in)
	if diff := cmp.Diff(&BodyError{Pointer: removalPointer, Message: `service account "bot" is not an id; name the service account by its metadata.id`}, err); diff != "" {
		t.Errorf("slug (-want +got):\n%s", diff)
	}
}

// An unbinding proposes only the binding it left, and is listed as no binding made; a removal proposes its row deleted and what it took along.
func TestProposal_UnbindingAndRemoval(t *testing.T) {
	w := newRemovalWorld()
	shrunk := *w.tierBind
	shrunk.Spec.AccountIDs = []string{"other"}
	rb := &AccountRebinding{Account: w.lane, Held: w.held}
	rows := []Row{
		{Kind: "policy-binding", Pointer: removalPointer, Meta: &rb.Binding.Meta, Value: rb},
		removalRow("account", &w.share.Meta, &Removal{Row: w.share, Along: []Row{{Kind: "policy-binding", Meta: &shrunk.Meta, Value: &shrunk, Op: OpUpdate}}}),
		removalRow("service-account", &w.sa.Meta, &Removal{Row: w.sa, Along: []Row{removalRow("account", &w.lane.Meta, w.lane)}}),
	}
	want := []any{plan.Deleted{Row: w.share}, plan.Deleted{Row: w.sa}, plan.Deleted{Row: w.held}, &shrunk, plan.Deleted{Row: w.lane}}
	if diff := cmp.Diff(want, proposal(rows)); diff != "" {
		t.Errorf("proposal (-want +got):\n%s", diff)
	}
	if got := Changed(rows, OpCreate); len(got) != 0 {
		t.Errorf("an unbinding lists %d rows made", len(got))
	}
}
