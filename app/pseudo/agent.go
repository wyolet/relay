// The agent pseudo kind: a service account with its lane account, what it may use, and its first key.

package pseudo

import (
	"fmt"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// AgentLabel is the label carrying the service account's id on the pools and policy made for it.
const AgentLabel = "agent-id"

// Agent is the body of POST /pseudo-agents.
type Agent struct {
	Metadata AgentMetadata `json:"metadata" doc:"The service account's metadata."`
	Spec     AgentSpec     `json:"spec"     doc:"What the service account may use, and its first key."`
}

// AgentMetadata is the service account's metadata.
type AgentMetadata struct {
	Name        string            `json:"name,omitempty"        doc:"Slug of the service account. Omitted: derived from displayName, suffixed -2, -3, … while taken. A given name that is taken is refused as POST /service-accounts refuses it."`
	DisplayName string            `json:"displayName,omitempty" doc:"Free-text name shown in the UI."`
	Description string            `json:"description,omitempty" doc:"Free-text description of the service account."`
	Labels      map[string]string `json:"labels,omitempty"      doc:"Labels of the service account."`
	Owner       AgentOwner        `json:"owner,omitempty"       doc:"The scope that owns the service account; required, with no default. The pools, policy and policy binding made for it are owned by the same scope."`
}

// AgentOwner is the scope a service account is owned by; its kinds are serviceaccount.OwnerKinds.
type AgentOwner struct {
	Kind meta.OwnerKind `json:"kind,omitempty" doc:"Kind of the owning scope: one a service account may be owned by."`
	ID   string         `json:"id,omitempty"   doc:"Id of the owning scope."`
}

// Owner is o as an owner.
func (o AgentOwner) Owner() meta.Owner { return meta.Owner{Kind: o.Kind, ID: o.ID} }

// TransformSchema lists the owner kinds a service account takes, so the documented choices follow serviceaccount.OwnerKinds.
func (AgentOwner) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	if kind := s.Properties["kind"]; kind != nil {
		kind.Enum = nil
		for _, k := range serviceaccount.OwnerKinds {
			kind.Enum = append(kind.Enum, string(k))
		}
	}
	return s
}

// AgentSpec is what the service account may use, through an existing policy or one made for it, and its first key.
type AgentSpec struct {
	PolicyID string       `json:"policyId,omitempty" doc:"Id (metadata.id) of an existing policy to bind the service account's account to; a slug answers 400. Exactly one of policyId and access."`
	Access   *AgentAccess `json:"access,omitempty"   doc:"What the service account may use, made into a policy of its own. Exactly one of policyId and access."`
	Key      *AgentKey    `json:"key,omitempty"      doc:"The service account's first key. Omitted makes no key."`
}

// AgentAccess is what the service account may use.
type AgentAccess struct {
	EveryCredential bool     `json:"everyCredential,omitempty" doc:"True uses every credential the service account's account reaches by default; accountIds and poolIds must then be empty. With models, a policy of those models linking no pool is bound; without, nothing is."`
	PoolIDs         []string `json:"poolIds,omitempty"         doc:"Ids of existing pools the policy links first, in the order given, before the pools made from accountIds; each must be one a policy of the owner may link. Name a keyless host's pool here: its accounts are never pooled from accountIds. Unless everyCredential is true, poolIds or accountIds must not be empty."`
	AccountIDs      []string `json:"accountIds,omitempty"      doc:"Ids of the provider accounts the service account draws on, in order of preference. Each host gets one pool listing its accounts in the order given; the made pools follow poolIds in the policy, in the order each host first appears here. An account the owner reaches, or a share owned by the owner, is pooled as it is; an account the owner does not reach (the caller's personal account, an account outside the owner's team) is pooled through the owner's share of it, made first when there is none. An account at a host that takes no key answers 400: name that host's pool in poolIds."`
	Models          []string `json:"models,omitempty"          doc:"Models the service account may use, in the policy model grammar (provider or provider/model). Omitted allows every model the pools serve."`
}

// AgentKey is the service account's first key.
type AgentKey struct {
	DisplayName           string     `json:"displayName,omitempty"           doc:"Free-text name of the key; its slug is derived from it."`
	ExpiresAt             *time.Time `json:"expiresAt,omitempty"             doc:"When the key stops authenticating. Omitted or null never expires."`
	PassthroughAllowed    bool       `json:"passthroughAllowed,omitempty"    doc:"Let the key send its own upstream credential in proxy mode."`
	PayloadLoggingEnabled bool       `json:"payloadLoggingEnabled,omitempty" doc:"Capture request and response bodies of this key's requests."`
}

// AgentReads is what expanding an agent reads besides its body.
type AgentReads struct {
	// Accounts are the accounts access.accountIds names, by id.
	Accounts map[string]*account.Account
	// HostNames are the names of those accounts' hosts, by id.
	HostNames map[string]string
	// KeylessHosts are those of the hosts that take no key (host.Spec.NoAuth), by id.
	KeylessHosts map[string]bool
	// Reaches reports whether a pool of the owner may list the real account a without a share.
	Reaches func(a *account.Account) bool
	// Shares are the owner's shares of real accounts, by the real account's id: an account already shared with the owner is pooled through that share.
	Shares map[string]*account.Account
	// OwnerName is the name of the owner scope; a share's name carries it.
	OwnerName string
	// PolicyName is the name of the policy policyId names; the binding's name carries it.
	PolicyName string
	Names      *Namer
	NewID      func() string
}

// ExpandAgent returns the rows an agent body stands for, in write order: the service account, its lane account, then for policyId a binding of the lane to it; for access the shares and pools it needs, its policy (linking poolIds, then the pools made) and the binding; then the key.
func ExpandAgent(body Agent, in AgentReads) ([]Row, error) {
	if err := CheckAgent(body); err != nil {
		return nil, err
	}
	x := agentExpansion{in: in, owner: body.Metadata.Owner.Owner()}
	sa := x.serviceAccount(body.Metadata)
	lane := x.lane(sa)
	switch a := body.Spec.Access; {
	case a == nil:
		x.binding(body.Spec.PolicyID, in.PolicyName, lane, "/spec/policyId")
	case a.EveryCredential && len(a.Models) == 0:
	default:
		made, err := x.pools(sa, a.AccountIDs)
		if err != nil {
			return nil, err
		}
		p := x.policy(sa, append(slices.Clone(a.PoolIDs), made...), a.Models)
		x.binding(p.Meta.ID, p.Meta.Name, lane, "/spec/access")
	}
	if k := body.Spec.Key; k != nil {
		x.key(sa, lane, k)
	}
	return x.rows, nil
}

// CheckAgent refuses a body that names no owner, its access twice, not at all, or half, or a row by anything but its id; it needs no read, so a caller runs it before reading what the body names. ExpandAgent runs it too.
func CheckAgent(body Agent) error {
	if o := body.Metadata.Owner; o.Kind == "" || o.ID == "" {
		return &BodyError{Pointer: "/metadata/owner", Message: "metadata.owner is required: the kind and id of the scope that owns the service account"}
	}
	s := body.Spec
	if (s.PolicyID == "") == (s.Access == nil) {
		return &BodyError{Pointer: "/spec", Message: "exactly one of spec.policyId and spec.access is required"}
	}
	if s.PolicyID != "" && !ids.Valid(s.PolicyID) {
		return &BodyError{Pointer: "/spec/policyId", Message: fmt.Sprintf("policy %q is not an id; name the policy by its metadata.id", s.PolicyID)}
	}
	if s.Access == nil {
		return nil
	}
	return checkAccess(s.Access)
}

// listedOnce refuses an entry of the list at field that is not an id, or names a row twice.
func listedOnce(kind, field string, list []string) error {
	seen := map[string]bool{}
	for i, id := range list {
		if !ids.Valid(id) {
			return &BodyError{Pointer: fmt.Sprintf("/spec/access/%s/%d", field, i), Message: fmt.Sprintf("%s %q is not an id; name the %s by its metadata.id", kind, id, kind)}
		}
		if seen[id] {
			return &BodyError{Pointer: fmt.Sprintf("/spec/access/%s/%d", field, i), Message: fmt.Sprintf("%s %q is listed twice", kind, id)}
		}
		seen[id] = true
	}
	return nil
}

type agentExpansion struct {
	in    AgentReads
	owner meta.Owner
	rows  []Row
}

func (x *agentExpansion) add(kind, pointer string, m *meta.Metadata, v any) {
	x.rows = append(x.rows, Row{Kind: kind, Pointer: pointer, Meta: m, Value: v})
}

func (x *agentExpansion) serviceAccount(md AgentMetadata) *serviceaccount.ServiceAccount {
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: x.in.NewID(), Name: md.Name, DisplayName: md.DisplayName, Description: md.Description, Labels: md.Labels}}
	sa.SetOwner(x.owner)
	if sa.Meta.Name == "" {
		sa.Meta.Name = x.in.Names.Mint("service-account", md.DisplayName, "service-account")
	} else {
		x.in.Names.Hold("service-account", sa.Meta.Name)
	}
	x.add("service-account", "/metadata", &sa.Meta, sa)
	return sa
}

func (x *agentExpansion) lane(sa *serviceaccount.ServiceAccount) *account.Account {
	lane := account.ForServiceAccountIn(sa.Meta.ID, sa.Meta.Name, sa.Meta.DisplayName, sa.Meta.Owner)
	lane.Meta.ID = x.in.NewID()
	lane.Meta.Name = x.in.Names.Mint("account", lane.Meta.Name, "account")
	x.add("account", "/metadata", &lane.Meta, lane)
	return lane
}

// pools makes one pool per host of the listed accounts, in the order the hosts first appear, sharing first each real account the owner neither reaches nor holds a share of; it returns the pools' ids.
func (x *agentExpansion) pools(sa *serviceaccount.ServiceAccount, accountIDs []string) ([]string, error) {
	src := accountSources{accounts: x.in.Accounts, hostNames: x.in.HostNames, keyless: x.in.KeylessHosts, reaches: x.in.Reaches, shares: x.in.Shares, ownerName: x.in.OwnerName, names: x.in.Names, newID: x.in.NewID}
	hosts, members, err := poolMembers(src, x.owner, accountIDs, x.add)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(hosts))
	for _, h := range hosts {
		display := fmt.Sprintf("%s at %s", label(sa.Meta), x.in.HostNames[h])
		p := &pool.Pool{
			Meta: meta.Metadata{ID: x.in.NewID(), Name: x.in.Names.Mint("pool", display, "pool"), DisplayName: display, Owner: x.owner, Labels: x.agentLabels(sa)},
			Spec: pool.Spec{HostID: h, AccountIDs: members[h]},
		}
		x.add("pool", "/spec/access/accountIds", &p.Meta, p)
		ids = append(ids, p.Meta.ID)
	}
	return ids, nil
}

func (x *agentExpansion) policy(sa *serviceaccount.ServiceAccount, poolIDs, models []string) *policy.Policy {
	display := label(sa.Meta)
	p := &policy.Policy{
		Meta: meta.Metadata{ID: x.in.NewID(), Name: x.in.Names.Mint("policy", display, "policy"), DisplayName: display, Owner: x.owner, Labels: x.agentLabels(sa)},
		Spec: policy.Spec{PoolIDs: poolIDs, Models: models},
	}
	x.add("policy", "/spec/access", &p.Meta, p)
	return p
}

// binding binds the lane alone to the policy in the lane's scope, as PUT /accounts/{ref}/policy does.
func (x *agentExpansion) binding(policyID, policyName string, lane *account.Account, pointer string) {
	b := bindingAlone(x.in.Names, x.in.NewID, policyID, policyName, lane)
	x.add("policy-binding", pointer, &b.Meta, b)
}

// bindingAlone is a binding of acc alone to policyID in acc's scope, named as PUT /accounts/{ref}/policy names it.
func bindingAlone(names *Namer, newID func() string, policyID, policyName string, acc *account.Account) *policybinding.PolicyBinding {
	return &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: newID(), Name: names.Mint("policy-binding", policyName+"-"+acc.Meta.Name, "policy-binding"), Owner: acc.Meta.Owner},
		Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: []string{acc.Meta.ID}},
	}
}

func (x *agentExpansion) key(sa *serviceaccount.ServiceAccount, lane *account.Account, k *AgentKey) {
	row := &key.Key{
		Meta: meta.Metadata{ID: x.in.NewID(), Name: x.in.Names.Mint("key", k.DisplayName, "key"), DisplayName: k.DisplayName},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, AccountID: lane.Meta.ID,
			ExpiresAt: k.ExpiresAt, PassthroughAllowed: k.PassthroughAllowed, PayloadLoggingEnabled: k.PayloadLoggingEnabled,
		},
	}
	x.add("key", "/spec/key", &row.Meta, row)
}

func (x *agentExpansion) agentLabels(sa *serviceaccount.ServiceAccount) map[string]string {
	return map[string]string{AgentLabel: sa.Meta.ID}
}

// label is a row's display name, or its name.
func label(m meta.Metadata) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Name
}
