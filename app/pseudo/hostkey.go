// The host key pseudo kind: a provider key with the account it belongs to, the account's tier, and who the account is shared with.

package pseudo

import (
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// HostKeyForm is the body of POST /pseudo-host-keys.
type HostKeyForm struct {
	Metadata HostKeyFormMetadata `json:"metadata" doc:"The host key's metadata; its owner owns the account too."`
	Spec     HostKeyFormSpec     `json:"spec"     doc:"The key's value, the account it belongs to, the account's tier and who the account is shared with."`
}

// HostKeyFormMetadata is the host key's metadata.
type HostKeyFormMetadata struct {
	Name        string            `json:"name,omitempty"        doc:"Slug of the host key. Omitted: derived from displayName, suffixed -2, -3, … while taken."`
	DisplayName string            `json:"displayName,omitempty" doc:"Free-text name of the host key."`
	Description string            `json:"description,omitempty" doc:"Free-text description of the host key."`
	Labels      map[string]string `json:"labels,omitempty"      doc:"Labels of the host key."`
	Owner       meta.Owner        `json:"owner"                 doc:"The scope that owns the host key and its account, as POST /host-keys takes it; a user owner without an id is the caller."`
}

// HostKeyFormSpec is the key's value, its account, the account's tier and who the account is shared with.
type HostKeyFormSpec struct {
	HostID          string              `json:"hostId"                    doc:"Id (metadata.id) of the host the key authenticates to; a slug answers 400."`
	Value           string              `json:"value,omitempty"           writeOnly:"true" doc:"The secret, checked against the host, then sealed under the owner's scope and never returned. Exactly one of value and valueFrom; both omitted only for a host that takes no key, and then no host key is made."`
	ValueFrom       *hostkey.ValueFrom  `json:"valueFrom,omitempty"       doc:"Where the secret comes from, as a host key's valueFrom takes it, such as {kind: env, env: VAR}; it is not checked against the host. Exactly one of value and valueFrom."`
	PricingStrategy string              `json:"pricingStrategy,omitempty" enum:"api,sub" doc:"How the key's spend is accounted, as POST /host-keys takes it. Omitted means api."`
	AccountID       string              `json:"accountId,omitempty"       doc:"Id (metadata.id) of an existing provider account of the owner at the host to add the key to; a slug answers 400. Exactly one of accountId and account."`
	Account         *HostKeyFormAccount `json:"account,omitempty"         doc:"A new provider account of the owner at the host, made for the key. Exactly one of accountId and account."`
	TierPolicyID    string              `json:"tierPolicyId,omitempty"    doc:"Id (metadata.id) of one of the host's tier presets (GET /hosts/{ref}/policies) to bind the account to alone, as PUT /accounts/{ref}/policy binds it; a slug answers 400. Omitted leaves the account's binding as it is."`
	ShareWith       []account.Receiver  `json:"shareWith,omitempty"       doc:"Who the account is shared with, as POST /accounts/{ref}/shares takes a receiver, by id; a slug answers 400. A receiver that already has a share of the account, or reaches it by ownership, gets none and is answered with existing: true."`
}

// HostKeyFormAccount is the new account a key belongs to.
type HostKeyFormAccount struct {
	DisplayName string `json:"displayName,omitempty" doc:"Free-text name of the account; its slug is derived from it."`
}

// Organization is a provider organization a host named for a key.
type Organization struct {
	ID, Name string
}

// HostKeyReads is what expanding a host key reads besides its body.
type HostKeyReads struct {
	// Account is the account accountId names; nil for a new one.
	Account *account.Account
	// Holding is the binding holding Account; nil when none does.
	Holding *policybinding.PolicyBinding
	// OwnerAccounts are the owner's provider accounts at the host that the caller sees.
	OwnerAccounts []*account.Account
	HostName      string
	// Keyless reports whether the host takes no key (host.Spec.NoAuth).
	Keyless bool
	// Tier is the policy tierPolicyId names; nil when it is missing or the caller may not see it, which the binding's own checks answer.
	Tier *policy.Policy
	// Detected is the organization the host named for the value; nil when it named none.
	Detected *Organization
	// Receivers are where each shareWith entry stands, in order.
	Receivers []ShareReceiver
	// HostKeyID is the host key's id, minted before the transaction: its value is sealed under it first.
	HostKeyID string
	Names     *Namer
	NewID     func() string
}

// ShareReceiver is the scope a shareWith entry names and where it stands with the account.
type ShareReceiver struct {
	// Owner is the owner a share with it takes: the receiver itself, or a service account's project. Name names it in the share's name.
	Owner meta.Owner
	Name  string
	// Share is the receiver's share of the account when it holds one already.
	Share *account.Account
	// Reaches is set when the receiver reaches the account by ownership, so a share would add nothing.
	Reaches bool
}

// OrganizationError refuses a key whose provider organization does not fit the account it would join: Holder, another account of the owner at the host, holds the organization, or, with no Holder, the account names another one.
type OrganizationError struct {
	Pointer string
	Holder  *account.Account
	Message string
}

func (e *OrganizationError) Error() string { return e.Message }

// ExpandHostKey returns the rows a host key body stands for, in write order: the new account, the account's tier binding, the host key, and a share with each receiver that neither holds one nor reaches the account.
func ExpandHostKey(body HostKeyForm, in HostKeyReads) ([]Row, error) {
	s := body.Spec
	if err := CheckHostKey(body); err != nil {
		return nil, err
	}
	if err := CheckHostKeyValue(s, in.HostName, in.Keyless); err != nil {
		return nil, err
	}
	owner := body.Metadata.Owner
	if t := in.Tier; t != nil && t.Meta.Owner != (meta.Owner{Kind: meta.OwnerHost, ID: s.HostID}) {
		return nil, &BodyError{Pointer: "/spec/tierPolicyId", Message: fmt.Sprintf("policy %q is not a tier preset of host %q; GET /hosts/{ref}/policies lists them", t.Meta.Name, in.HostName)}
	}
	if a := in.Account; a != nil && !(a.IsProvider() && !a.IsShared() && a.Spec.HostID == s.HostID && sameOwner(a.Meta.Owner, owner)) {
		return nil, &BodyError{Pointer: "/spec/accountId", Message: fmt.Sprintf("account %q is not a provider account of the key's owner at host %q", a.Meta.Name, in.HostName)}
	}
	if err := checkOrganization(in); err != nil {
		return nil, err
	}
	if len(in.Receivers) != len(s.ShareWith) {
		return nil, fmt.Errorf("pseudo: %d receivers read for %d shareWith entries", len(in.Receivers), len(s.ShareWith))
	}
	x := hostKeyExpansion{in: in, owner: owner}
	acc := in.Account
	if acc == nil {
		acc = x.account(s)
	}
	if s.TierPolicyID != "" {
		x.tier(acc, s.TierPolicyID)
	}
	if s.Value != "" || s.ValueFrom != nil {
		x.hostKey(body.Metadata, s, acc)
	}
	for i, r := range s.ShareWith {
		if rc := in.Receivers[i]; rc.Share == nil && !rc.Reaches {
			x.share(acc, r, rc, fmt.Sprintf("/spec/shareWith/%d", i))
		}
	}
	return x.rows, nil
}

// CheckHostKey refuses a body that names a row by anything but its id, its account twice or not at all, its value twice, or a receiver twice; it needs no read, so a caller runs it before reading what the body names. ExpandHostKey runs it too.
func CheckHostKey(body HostKeyForm) error {
	s := body.Spec
	if id := body.Metadata.Owner.ID; id != "" && !ids.Valid(id) {
		return notAnID("/metadata/owner/id", "owner", id)
	}
	for _, f := range []struct{ pointer, kind, id string }{{"/spec/hostId", "host", s.HostID}, {"/spec/accountId", "account", s.AccountID}, {"/spec/tierPolicyId", "policy", s.TierPolicyID}} {
		if (f.id != "" || f.kind == "host") && !ids.Valid(f.id) {
			return notAnID(f.pointer, f.kind, f.id)
		}
	}
	if (s.AccountID == "") == (s.Account == nil) {
		return &BodyError{Pointer: "/spec", Message: "exactly one of spec.accountId and spec.account is required"}
	}
	if s.Value != "" && s.ValueFrom != nil {
		return &BodyError{Pointer: "/spec", Message: "spec.value and spec.valueFrom exclude each other; send one"}
	}
	seen := map[account.Receiver]bool{}
	for i, r := range s.ShareWith {
		if !ids.Valid(r.ID) {
			return notAnID(fmt.Sprintf("/spec/shareWith/%d/id", i), "receiver", r.ID)
		}
		if seen[r] {
			return &BodyError{Pointer: fmt.Sprintf("/spec/shareWith/%d", i), Message: fmt.Sprintf("receiver %s %q is listed twice", r.Kind, r.ID)}
		}
		seen[r] = true
	}
	return nil
}

func notAnID(pointer, kind, id string) error {
	return &BodyError{Pointer: pointer, Message: fmt.Sprintf("%s %q is not an id; name the %s by its metadata.id", kind, id, kind)}
}

// CheckHostKeyValue refuses a value for a host that takes no key, and none for a host that takes one.
func CheckHostKeyValue(s HostKeyFormSpec, hostName string, keyless bool) error {
	switch {
	case keyless && s.Value != "":
		return &BodyError{Pointer: "/spec/value", Message: fmt.Sprintf("host %q takes no key; leave spec.value out", hostName)}
	case keyless && s.ValueFrom != nil:
		return &BodyError{Pointer: "/spec/valueFrom", Message: fmt.Sprintf("host %q takes no key; leave spec.valueFrom out", hostName)}
	case !keyless && s.Value == "" && s.ValueFrom == nil:
		return &BodyError{Pointer: "/spec", Message: fmt.Sprintf("host %q takes a key: exactly one of spec.value and spec.valueFrom is required", hostName)}
	}
	return nil
}

// checkOrganization refuses a detected organization another account of the owner at the host holds, or one the account named does not hold while it names another.
func checkOrganization(in HostKeyReads) error {
	d, target := in.Detected, in.Account
	if d == nil || (target != nil && target.Spec.OrgID == d.ID) {
		return nil
	}
	pointer := "/spec/account"
	if target != nil {
		pointer = "/spec/accountId"
	}
	for _, a := range in.OwnerAccounts {
		if a.Spec.OrgID == d.ID && (target == nil || a.Meta.ID != target.Meta.ID) {
			return &OrganizationError{Pointer: pointer, Holder: a, Message: fmt.Sprintf("the key belongs to organization %q, which account %q of the owner holds; send accountId %s to add the key there", d.ID, a.Meta.Name, a.Meta.ID)}
		}
	}
	if target != nil && target.Spec.OrgID != "" {
		return &OrganizationError{Pointer: pointer, Message: fmt.Sprintf("the key belongs to organization %q, but account %q is organization %q", d.ID, target.Meta.Name, target.Spec.OrgID)}
	}
	return nil
}

// sameOwner reports whether a key of keyOwner may join an account of accountOwner, as refcheck's join rule has it.
func sameOwner(accountOwner, keyOwner meta.Owner) bool {
	return accountOwner == keyOwner || (accountOwner.DeploymentWide() && keyOwner.DeploymentWide())
}

type hostKeyExpansion struct {
	in    HostKeyReads
	owner meta.Owner
	rows  []Row
}

func (x *hostKeyExpansion) add(kind, pointer string, m *meta.Metadata, v any) {
	x.rows = append(x.rows, Row{Kind: kind, Pointer: pointer, Meta: m, Value: v})
}

func (x *hostKeyExpansion) account(s HostKeyFormSpec) *account.Account {
	display := s.Account.DisplayName
	a := &account.Account{
		Meta: meta.Metadata{ID: x.in.NewID(), Name: x.in.Names.Mint("account", display, "account"), DisplayName: display, Owner: x.owner},
		Spec: account.Spec{HostID: s.HostID},
	}
	if d := x.in.Detected; d != nil {
		a.Spec.OrgID = d.ID
	}
	x.add("account", "/spec/account", &a.Meta, a)
	return a
}

// tier binds acc alone to policyID: a new account in a binding of its own, an existing one through the PUT /accounts/{ref}/policy write unless an enabled binding of policyID holds it already.
func (x *hostKeyExpansion) tier(acc *account.Account, policyID string) {
	if acc != x.in.Account {
		name := "policy"
		if t := x.in.Tier; t != nil {
			name = t.Meta.Name
		}
		b := &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: x.in.NewID(), Name: x.in.Names.Mint("policy-binding", name+"-"+acc.Meta.Name, "policy-binding"), Owner: acc.Meta.Owner},
			Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: []string{acc.Meta.ID}},
		}
		x.add("policy-binding", "/spec/tierPolicyId", &b.Meta, b)
		return
	}
	if h := x.in.Holding; h != nil && h.Spec.PolicyID == policyID && h.IsEnabled() {
		return
	}
	rb := &AccountRebinding{Account: acc, PolicyID: policyID}
	x.add("policy-binding", "/spec/tierPolicyId", &rb.Binding.Meta, rb)
}

func (x *hostKeyExpansion) hostKey(md HostKeyFormMetadata, s HostKeyFormSpec, acc *account.Account) {
	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: x.in.HostKeyID, Name: md.Name, DisplayName: md.DisplayName, Description: md.Description, Labels: md.Labels, Owner: x.owner},
		Spec: hostkey.Spec{HostID: s.HostID, AccountID: acc.Meta.ID, PricingStrategy: s.PricingStrategy},
	}
	if s.ValueFrom != nil {
		k.Spec.ValueFrom = *s.ValueFrom
	} else {
		k.Spec.ValueFrom, k.Spec.Value = hostkey.ValueFrom{Kind: hostkey.ValueKindStored}, s.Value
	}
	if k.Meta.Name == "" {
		k.Meta.Name = x.in.Names.Mint("host-key", md.DisplayName, "host-key")
	} else {
		x.in.Names.Hold("host-key", k.Meta.Name)
	}
	x.add("host-key", "/metadata", &k.Meta, k)
}

func (x *hostKeyExpansion) share(src *account.Account, r account.Receiver, rc ShareReceiver, pointer string) {
	s := account.Share(src, r, rc.Owner, rc.Name)
	s.Meta.ID = x.in.NewID()
	s.Meta.Name = x.in.Names.Mint("account", s.Meta.Name, "account")
	x.add("account", pointer, &s.Meta, s)
}
