package pseudo

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

var (
	proj      = meta.Owner{Kind: meta.OwnerProject, ID: "proj"}
	alice     = meta.Owner{Kind: meta.OwnerUser, ID: "alice"}
	teamOwner = meta.Owner{Kind: meta.OwnerTeam, ID: "team"}
)

// Ids of the rows a body names.
const (
	teamAcctID  = "01a20b40-0000-7000-8000-000000000001"
	projShareID = "01a20b40-0000-7000-8000-000000000002"
	aliceAcctID = "01a20b40-0000-7000-8000-000000000003"
	bobAcctID   = "01a20b40-0000-7000-8000-000000000004"
	bobShareID  = "01a20b40-0000-7000-8000-000000000005"
	policyID    = "01a20b40-0000-7000-8000-000000000006"
	localPoolID = "01a20b40-0000-7000-8000-000000000007"
	otherPoolID = "01a20b40-0000-7000-8000-000000000008"
	nowhereID   = "01a20b40-0000-7000-8000-000000000009"
)

// agentWorld is what an expansion reads: a team account at host a the project reaches, a share already owned by the project at host a, alice's personal account at host b it does not reach, and bob's at host b, already shared with the project.
type agentWorld struct {
	teamAcct, projShare, alicesAcct, bobsAcct, bobsShare *account.Account
	taken                                                map[string]bool
}

func newAgentWorld() *agentWorld {
	bob := meta.Owner{Kind: meta.OwnerUser, ID: "bob"}
	return &agentWorld{
		teamAcct:   &account.Account{Meta: meta.Metadata{ID: teamAcctID, Name: "team-a", Owner: teamOwner}, Spec: account.Spec{HostID: "host-a"}},
		projShare:  &account.Account{Meta: meta.Metadata{ID: projShareID, Name: "shared-a", Owner: proj}, Spec: account.Spec{HostID: "host-a", SourceAccountID: "elsewhere"}},
		alicesAcct: &account.Account{Meta: meta.Metadata{ID: aliceAcctID, Name: "alice-b", DisplayName: "Alice B", Owner: alice}, Spec: account.Spec{HostID: "host-b"}},
		bobsAcct:   &account.Account{Meta: meta.Metadata{ID: bobAcctID, Name: "bob-b", Owner: bob}, Spec: account.Spec{HostID: "host-b"}},
		bobsShare:  &account.Account{Meta: meta.Metadata{ID: bobShareID, Name: "bob-b-for-app", Owner: proj}, Spec: account.Spec{HostID: "host-b", SourceAccountID: bobAcctID}},
		taken:      map[string]bool{"service-account/ci-bot": true},
	}
}

func (w *agentWorld) reads() AgentReads {
	n := 0
	return AgentReads{
		Accounts:   map[string]*account.Account{w.teamAcct.Meta.ID: w.teamAcct, w.projShare.Meta.ID: w.projShare, w.alicesAcct.Meta.ID: w.alicesAcct, w.bobsAcct.Meta.ID: w.bobsAcct},
		Shares:     map[string]*account.Account{w.bobsAcct.Meta.ID: w.bobsShare},
		HostNames:  map[string]string{"host-a": "alpha", "host-b": "beta"},
		Reaches:    func(a *account.Account) bool { return a.Meta.Owner == teamOwner },
		OwnerName:  "app",
		PolicyName: "gold",
		Names:      &Namer{Taken: func(kind, name string) bool { return w.taken[kind+"/"+name] }},
		NewID:      func() string { n++; return "id-" + strconv.Itoa(n) },
	}
}

func agentBody(spec AgentSpec) Agent {
	return Agent{Metadata: AgentMetadata{DisplayName: "CI bot", Labels: map[string]string{"sa-type": "agent"}, Owner: AgentOwner{Kind: proj.Kind, ID: proj.ID}}, Spec: spec}
}

// An owner is required; there is no default.
func TestExpandAgent_RefusesNoOwner(t *testing.T) {
	body := agentBody(AgentSpec{PolicyID: policyID})
	body.Metadata.Owner = AgentOwner{}
	rows, err := ExpandAgent(body, newAgentWorld().reads())
	want := &BodyError{Pointer: "/metadata/owner", Message: "metadata.owner is required: the kind and id of the scope that owns the service account"}
	if diff := cmp.Diff(want, err); diff != "" || rows != nil {
		t.Errorf("rows %v, error (-want +got):\n%s", rows, diff)
	}
}

// The rows every agent starts with: the service account (id-1, its name taken once) and its lane (id-2).
func headRows() []Row {
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: "id-1", Name: "ci-bot-2", DisplayName: "CI bot", Labels: map[string]string{"sa-type": "agent"}, Owner: proj}, Spec: serviceaccount.Spec{ProjectID: "proj"}}
	lane := account.ForServiceAccountIn("id-1", "ci-bot-2", "CI bot", proj)
	lane.Meta.ID = "id-2"
	return []Row{
		{Kind: "service-account", Pointer: "/metadata", Meta: &sa.Meta, Value: sa},
		{Kind: "account", Pointer: "/metadata", Meta: &lane.Meta, Value: lane},
	}
}

func bindingRow(id, name, policyID, pointer string) Row {
	b := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: id, Name: name, Owner: proj}, Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: []string{"id-2"}}}
	return Row{Kind: "policy-binding", Pointer: pointer, Meta: &b.Meta, Value: b}
}

func policyRow(id string, poolIDs, models []string) Row {
	p := &policy.Policy{Meta: meta.Metadata{ID: id, Name: "ci-bot", DisplayName: "CI bot", Owner: proj, Labels: map[string]string{AgentLabel: "id-1"}}, Spec: policy.Spec{PoolIDs: poolIDs, Models: models}}
	return Row{Kind: "policy", Pointer: "/spec/access", Meta: &p.Meta, Value: p}
}

func keyRow(id string, expires *time.Time) Row {
	k := &key.Key{
		Meta: meta.Metadata{ID: id, Name: "ci-bot-key", DisplayName: "CI bot key"},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: "id-1"}, AccountID: "id-2", ExpiresAt: expires, PayloadLoggingEnabled: true},
	}
	return Row{Kind: "key", Pointer: "/spec/key", Meta: &k.Meta, Value: k}
}

func TestExpandAgent(t *testing.T) {
	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	firstKey := &AgentKey{DisplayName: "CI bot key", ExpiresAt: &expires, PayloadLoggingEnabled: true}
	w := newAgentWorld()

	sharedAlice := account.Share(w.alicesAcct, account.Receiver{Kind: account.ReceiverProject, ID: "proj"}, proj, "app")
	sharedAlice.Meta.ID = "id-3"
	poolA := &pool.Pool{Meta: meta.Metadata{ID: "id-4", Name: "ci-bot-at-alpha", DisplayName: "CI bot at alpha", Owner: proj, Labels: map[string]string{AgentLabel: "id-1"}}, Spec: pool.Spec{HostID: "host-a", AccountIDs: []string{teamAcctID, projShareID}}}
	poolB := &pool.Pool{Meta: meta.Metadata{ID: "id-5", Name: "ci-bot-at-beta", DisplayName: "CI bot at beta", Owner: proj, Labels: map[string]string{AgentLabel: "id-1"}}, Spec: pool.Spec{HostID: "host-b", AccountIDs: []string{"id-3", bobShareID}}}

	for _, tc := range []struct {
		name string
		spec AgentSpec
		want []Row
	}{
		{
			name: "an existing policy, no key",
			spec: AgentSpec{PolicyID: policyID},
			want: append(headRows(), bindingRow("id-3", "gold-sa-ci-bot-2", policyID, "/spec/policyId")),
		},
		{
			name: "every credential, no models, a key",
			spec: AgentSpec{Access: &AgentAccess{EveryCredential: true}, Key: firstKey},
			want: append(headRows(), keyRow("id-3", &expires)),
		},
		{
			name: "every credential with models",
			spec: AgentSpec{Access: &AgentAccess{EveryCredential: true, Models: []string{"openai/gpt-5.5"}}},
			want: append(headRows(), policyRow("id-3", nil, []string{"openai/gpt-5.5"}), bindingRow("id-4", "ci-bot-sa-ci-bot-2", "id-3", "/spec/access")),
		},
		{
			name: "existing pools only",
			spec: AgentSpec{Access: &AgentAccess{PoolIDs: []string{localPoolID, otherPoolID}}},
			want: append(headRows(), policyRow("id-3", []string{localPoolID, otherPoolID}, nil), bindingRow("id-4", "ci-bot-sa-ci-bot-2", "id-3", "/spec/access")),
		},
		{
			name: "existing pools first, then one pool per host in the order hosts first appear; accounts reached, a share the owner holds, a real account it holds a share of, and one shared first",
			spec: AgentSpec{Access: &AgentAccess{PoolIDs: []string{localPoolID}, AccountIDs: []string{teamAcctID, aliceAcctID, projShareID, bobAcctID}, Models: []string{"openai"}}, Key: firstKey},
			want: append(headRows(),
				Row{Kind: "account", Pointer: "/spec/access/accountIds/1", Meta: &sharedAlice.Meta, Value: sharedAlice},
				Row{Kind: "pool", Pointer: "/spec/access/accountIds", Meta: &poolA.Meta, Value: poolA},
				Row{Kind: "pool", Pointer: "/spec/access/accountIds", Meta: &poolB.Meta, Value: poolB},
				policyRow("id-6", []string{localPoolID, "id-4", "id-5"}, []string{"openai"}),
				bindingRow("id-7", "ci-bot-sa-ci-bot-2", "id-6", "/spec/access"),
				keyRow("id-8", &expires)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandAgent(agentBody(tc.spec), newAgentWorld().reads())
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("rows (-want +got):\n%s", diff)
			}
		})
	}
}

// A given name is kept and held from the rows minted after it; minted names skip one another.
func TestExpandAgent_Names(t *testing.T) {
	body := agentBody(AgentSpec{PolicyID: policyID, Key: &AgentKey{}})
	body.Metadata.Name = "ci-bot"
	in := newAgentWorld().reads()
	in.Names = &Namer{Taken: func(kind, name string) bool { return kind == "key" && name == "key" }}
	rows, err := ExpandAgent(body, in)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Kind+"/"+r.Meta.Name)
	}
	want := []string{"service-account/ci-bot", "account/sa-ci-bot", "policy-binding/gold-sa-ci-bot", "key/key-2"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
}

func TestExpandAgent_RefusesBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec AgentSpec
		want *BodyError
	}{
		{"neither policyId nor access", AgentSpec{}, &BodyError{Pointer: "/spec", Message: "exactly one of spec.policyId and spec.access is required"}},
		{"both policyId and access", AgentSpec{PolicyID: policyID, Access: &AgentAccess{EveryCredential: true}}, &BodyError{Pointer: "/spec", Message: "exactly one of spec.policyId and spec.access is required"}},
		{"a policy by slug", AgentSpec{PolicyID: "gold"}, &BodyError{Pointer: "/spec/policyId", Message: `policy "gold" is not an id; name the policy by its metadata.id`}},
		{"an account by slug", AgentSpec{Access: &AgentAccess{AccountIDs: []string{teamAcctID, "team-a"}}}, &BodyError{Pointer: "/spec/access/accountIds/1", Message: `account "team-a" is not an id; name the account by its metadata.id`}},
		{"a pool by slug", AgentSpec{Access: &AgentAccess{PoolIDs: []string{"local"}}}, &BodyError{Pointer: "/spec/access/poolIds/0", Message: `pool "local" is not an id; name the pool by its metadata.id`}},
		{"every credential with accounts", AgentSpec{Access: &AgentAccess{EveryCredential: true, AccountIDs: []string{teamAcctID}}}, &BodyError{Pointer: "/spec/access/accountIds", Message: "accountIds must be empty when everyCredential is true"}},
		{"every credential with pools", AgentSpec{Access: &AgentAccess{EveryCredential: true, PoolIDs: []string{localPoolID}}}, &BodyError{Pointer: "/spec/access/poolIds", Message: "poolIds must be empty when everyCredential is true"}},
		{"neither accounts nor pools", AgentSpec{Access: &AgentAccess{}}, &BodyError{Pointer: "/spec/access", Message: "accountIds or poolIds is required unless everyCredential is true"}},
		{"an account twice", AgentSpec{Access: &AgentAccess{AccountIDs: []string{teamAcctID, teamAcctID}}}, &BodyError{Pointer: "/spec/access/accountIds/1", Message: `account "` + teamAcctID + `" is listed twice`}},
		{"a pool twice", AgentSpec{Access: &AgentAccess{PoolIDs: []string{localPoolID, otherPoolID, localPoolID}}}, &BodyError{Pointer: "/spec/access/poolIds/2", Message: `pool "` + localPoolID + `" is listed twice`}},
		{"an account not read", AgentSpec{Access: &AgentAccess{AccountIDs: []string{teamAcctID, nowhereID}}}, &BodyError{Pointer: "/spec/access/accountIds/1", Message: `account "` + nowhereID + `" not found`}},
		{"an account at a host that takes no key", AgentSpec{Access: &AgentAccess{AccountIDs: []string{teamAcctID, bobAcctID}}},
			&BodyError{Pointer: "/spec/access/accountIds/1", Message: `account "bob-b" is at host "beta", which takes no key, so a pool would ignore it; name that host's pool in poolIds instead`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newAgentWorld().reads()
			in.KeylessHosts = map[string]bool{"host-b": true}
			rows, err := ExpandAgent(agentBody(tc.spec), in)
			if diff := cmp.Diff(tc.want, err); diff != "" || rows != nil {
				t.Errorf("rows %v, error (-want +got):\n%s", rows, diff)
			}
		})
	}
}
