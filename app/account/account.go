// Package account is the domain layer for the Account entity: what policies bind to and usage is charged to. A provider account is one provider organization at one host, whose tier limits every host key of the organization shares. A caller account is one relay keeps in step with the tenancy rows: one per team and per project, a personal account per user, one per service account, and a member account per user and team or project the user is a member of. A key lands in one account its principal holds, its default lane. A shared account is a provider account owned by the user, team or project it was shared with (a service account's by its project, held by it) that holds no keys and reaches those of the real account it names.
//
// This package validates one row's shape, stores it, creates an owner's default provider account at a host on first use (Store.EnsureDefault), and creates a caller account idempotently (Store.EnsureManaged). Out of scope: learning org ids from upstream responses, deciding which caller accounts should exist (app/plan) and applying an account's policy to requests.
package account

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/slug"
)

// Account is a provider organization at one host, or a caller account.
type Account struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`

	// FollowsRoleBindings marks a member account made from a role binding: it closes when the last such grant goes. One made on first use, after a group grant relay cannot list, stays open.
	FollowsRoleBindings bool `json:"-" yaml:"-"`
}

// Kind is what an account is, derived from its host, holder and owner.
type Kind string

const (
	KindTeam     Kind = "team"
	KindProject  Kind = "project"
	KindPersonal Kind = "personal"
	KindMember   Kind = "member"
	KindService  Kind = "service"
	KindProvider Kind = "provider"
)

// HolderKind names the principal an account is held by.
type HolderKind string

const (
	HolderUser           HolderKind = "user"
	HolderServiceAccount HolderKind = "serviceaccount"
)

// Holder is the principal acting in an account.
type Holder struct {
	Kind HolderKind `json:"kind" yaml:"kind" validate:"required,oneof=user serviceaccount" enum:"user,serviceaccount" doc:"Whether a user or a service account acts in the account."`
	ID   string     `json:"id"   yaml:"id"   validate:"required,uuid" doc:"Id of the user or service account."`
	// Derived: filled on control API reads, never stored; compare holders by Kind and ID.
	Name string `json:"name,omitempty" yaml:"-" readOnly:"true" doc:"Username of the user, or display name of the service account. Read-only: filled on reads for a caller who sees the account; ignored on writes."`
}

// Key renders the holder as a principal string, "user:<id>" or "serviceaccount:<id>".
func (h *Holder) Key() string { return string(h.Kind) + ":" + h.ID }

// ReceiverKind names who a provider account is shared with.
type ReceiverKind string

const (
	ReceiverUser           ReceiverKind = "user"
	ReceiverTeam           ReceiverKind = "team"
	ReceiverProject        ReceiverKind = "project"
	ReceiverServiceAccount ReceiverKind = "serviceaccount"
)

// Receiver is who a provider account is shared with.
type Receiver struct {
	Kind ReceiverKind `json:"kind" yaml:"kind" validate:"required,oneof=user team project serviceaccount" enum:"user,team,project,serviceaccount" doc:"Whether the account is shared with a user, a team, a project or a service account."`
	ID   string       `json:"id"   yaml:"id"   validate:"required,uuid" doc:"Id of the user, team, project or service account."`
}

// Key renders the receiver as "<kind>:<id>", the form one share per receiver is keyed by.
func (r Receiver) Key() string { return string(r.Kind) + ":" + r.ID }

// ShareParty names one side of a share: the owner of the real account, or the receiver.
type ShareParty struct {
	Kind string `json:"kind" enum:"system,user,team,project,serviceaccount" doc:"Kind of the party: the system, a user, a team, a project or a service account."`
	ID   string `json:"id,omitempty"   doc:"Id of the user, team, project or service account; empty for the system."`
	Name string `json:"name,omitempty" doc:"Username, or slug of the team, project or service account; empty for the system or when the row is gone."`
}

// IsShared reports whether a is a shared account: one that reaches another provider account's keys and holds none of its own.
func (a *Account) IsShared() bool { return a.Spec.SourceAccountID != "" }

// Receiver is who a shared account was shared with: its holder for a service account's share, else its owner.
func (a *Account) Receiver() Receiver {
	if h := a.Spec.Holder; h != nil && h.Kind == HolderServiceAccount {
		return Receiver{Kind: ReceiverServiceAccount, ID: h.ID}
	}
	return Receiver{Kind: ReceiverKind(a.Meta.Owner.Kind), ID: a.Meta.Owner.ID}
}

// TakesKeysOf reports whether a relay key of principal may run in a. A key outlives the membership it was made under, so it lands only where relay re-checks that membership itself: the principal's personal or service account, a member account that follows role bindings, or a provider account the key's user owns personally. A member account opened on first use (a group or team-level grant relay cannot see) serves minted tokens only, and team, project and system provider accounts are reached through the caller ladder, never as a lane.
func (a *Account) TakesKeysOf(principal Holder) bool {
	switch a.DerivedKind() {
	case KindPersonal, KindService:
		return *a.Spec.Holder == principal
	case KindMember:
		return *a.Spec.Holder == principal && a.FollowsRoleBindings
	case KindProvider:
		return principal.Kind == HolderUser && a.Meta.Owner == meta.Owner{Kind: meta.OwnerUser, ID: principal.ID}
	}
	return false
}

// PayloadLogging is an account's say on capturing request and response bodies; empty leaves it to the account above on the ladder, then to the key.
type PayloadLogging string

const (
	PayloadLoggingOn  PayloadLogging = "on"
	PayloadLoggingOff PayloadLogging = "off"
)

// Spec carries the holder, the host and org of a provider account, and the account's flags.
type Spec struct {
	// Kind is derived on every write; a value sent is ignored.
	Kind Kind `json:"kind" yaml:"-" readOnly:"true" enum:"team,project,personal,member,service,provider" doc:"What the account is: a team's or a project's own account, a user's personal account, a user's member account in a team or project, a service account's account, or a provider account at a host. Derived from hostId, holder and the owner; server-managed."`

	// Holder is the principal acting in the account: set on personal, member and service accounts, empty on team, project and provider accounts. Fixed at create.
	Holder *Holder `json:"holder,omitempty" yaml:"holder,omitempty" readOnly:"true" doc:"The user or service account acting in the account. Set on personal, member and service accounts; empty on team, project and provider accounts. Server-managed."`

	// HostID is the host a provider account is held at; empty on caller accounts.
	HostID string `json:"hostId,omitempty" yaml:"hostId,omitempty" validate:"omitempty,uuid" doc:"Id of the host a provider account is held at. Set on provider accounts only."`

	// OrgID is the provider's organization id: the org, never a person or an email, since one login can belong to several orgs with separate limits. Empty until known.
	OrgID string `json:"orgId,omitempty" yaml:"orgId,omitempty" doc:"The provider's organization id (the org, never a person or an email). Provider accounts only; empty until known."`

	// Default marks the provider account a host key of the same owner at HostID joins when it names none. An owner has at most one per host.
	Default bool `json:"default,omitempty" yaml:"default,omitempty" doc:"The provider account a host key of the same owner at this host joins when it names none. Created on first use; an owner has at most one per host, and a second answers 409."`

	PayloadLogging PayloadLogging `json:"payloadLogging,omitempty" yaml:"payloadLogging,omitempty" validate:"omitempty,oneof=on off" enum:"on,off" doc:"Capture request and response bodies of requests in this account: on or off. Omitted leaves it to the account above it on the ladder (project, then team), then to the key's own flag. On a personal account only an admin sets it; on a team, project, member or service account an admin of its scope, never the member account's own holder (403)."`

	// Closed is set on a member account whose membership ended; its keys are refused.
	Closed bool `json:"closed,omitempty" yaml:"-" readOnly:"true" doc:"The membership this member account stood for has ended: its keys are refused with account_closed. Reopened when a role grants the membership again. Server-managed."`

	// SourceAccountID marks a shared account: the provider account whose keys it reaches. It holds no keys of its own, and is fixed at create.
	SourceAccountID string `json:"sourceAccountId,omitempty" yaml:"sourceAccountId,omitempty" readOnly:"true" validate:"omitempty,uuid" doc:"Set on a shared account: the id of the provider account whose keys it reaches. A shared account holds no host keys of its own; its requests use the real account's keys, count on the real account's tier, and only narrow by the policy bound to the shared account. Made by POST /accounts/{ref}/shares; fixed at create."`

	// SharedBy is derived on read from the real account's owner.
	SharedBy *ShareParty `json:"sharedBy,omitempty" yaml:"-" readOnly:"true" doc:"Set on a shared account: the owner of the real account, who shared it, always named to whoever sees the share (sharing it was their choice). Derived on read."`
}

// DerivedKind is the account's kind from its host, holder and owner; "" when they fit no kind.
func (a *Account) DerivedKind() Kind {
	o := a.Meta.Owner
	switch h := a.Spec.Holder; {
	case a.Spec.HostID != "":
		// A service account's share is owned by its project and held by it, so only its lane draws on it.
		if h == nil || (a.IsShared() && h.Kind == HolderServiceAccount && o.Kind == meta.OwnerProject) {
			return KindProvider
		}
	case h == nil:
		switch o.Kind {
		case meta.OwnerTeam:
			return KindTeam
		case meta.OwnerProject:
			return KindProject
		}
	case h.Kind == HolderUser:
		switch o.Kind {
		case meta.OwnerUser:
			if o.ID == h.ID {
				return KindPersonal
			}
		case meta.OwnerTeam, meta.OwnerProject:
			return KindMember
		}
	case h.Kind == HolderServiceAccount:
		if o.Kind == meta.OwnerProject {
			return KindService
		}
	}
	return ""
}

// IsProvider reports whether the account is a provider org at a host.
func (a *Account) IsProvider() bool { return a.Spec.HostID != "" }

// ManagedKey names a caller account's place, the identity concurrent writers converge on: "team:<id>", "project:<id>", "personal:<user>", "service:<sa>" or "member:<team|project>:<id>:<user>". "" for a provider account.
func (a *Account) ManagedKey() string {
	o, h := a.Meta.Owner, a.Spec.Holder
	switch a.DerivedKind() {
	case KindTeam, KindProject:
		return string(o.Kind) + ":" + o.ID
	case KindPersonal:
		return "personal:" + h.ID
	case KindService:
		return "service:" + h.ID
	case KindMember:
		return MemberKey(o, h.ID)
	}
	return ""
}

// MemberKey is the managed key of userID's member account in scope.
func MemberKey(scope meta.Owner, userID string) string {
	return "member:" + string(scope.Kind) + ":" + scope.ID + ":" + userID
}

// Validate runs intra-row rules via the shared meta.Validator, derives Spec.Kind, and enforces each kind's shape: a provider account has a host, an owner of kind system, team, project or user, and no holder; a caller account has no host, org id or default mark.
//
// Cross-row checks (the host and holder exist) live in app/refcheck.
func (a *Account) Validate() error {
	if err := meta.Validator.Struct(a); err != nil {
		return err
	}
	o := a.Meta.Owner
	switch o.Kind {
	case meta.OwnerSystem, meta.OwnerUser:
	case meta.OwnerTeam, meta.OwnerProject:
		if o.ID == "" {
			return fmt.Errorf("account %q: owner.kind %q requires an id", a.Meta.Name, o.Kind)
		}
	default:
		return fmt.Errorf("account %q: owner.kind must be system, team, project or user, got %q", a.Meta.Name, o.Kind)
	}
	k := a.DerivedKind()
	switch {
	case k == "" && a.Spec.HostID != "":
		return fmt.Errorf("account %q: a provider account (spec.hostId set) has no holder, except a service account's share, owned by its project", a.Meta.Name)
	case k == "":
		return fmt.Errorf("account %q: spec.hostId is required unless the account is a team's, a project's, a user's personal account, a member account or a service account's", a.Meta.Name)
	case k != KindProvider && (a.Spec.OrgID != "" || a.Spec.Default):
		return fmt.Errorf("account %q: orgId and default are for provider accounts only", a.Meta.Name)
	case a.IsShared() && k != KindProvider:
		return fmt.Errorf("account %q: only a provider account is shared", a.Meta.Name)
	case a.IsShared() && (a.Spec.OrgID != "" || a.Spec.Default):
		return fmt.Errorf("account %q: a shared account takes its org and keys from the real account; orgId and default stay on that one", a.Meta.Name)
	case a.IsShared() && o.Kind == meta.OwnerSystem:
		return fmt.Errorf("account %q: a shared account is owned by the user, team or project it was shared with", a.Meta.Name)
	case a.IsShared() && a.Spec.SourceAccountID == a.Meta.ID:
		return fmt.Errorf("account %q: an account cannot be shared with itself", a.Meta.Name)
	}
	a.Spec.Kind = k
	return nil
}

// Share is the account sharing src with r, owned by owner (r itself, or a service account's project) and named after src and receiverName; the store mints the final name.
func Share(src *Account, r Receiver, owner meta.Owner, receiverName string) *Account {
	name := slug.From(src.Meta.Name + "-for-" + receiverName)
	if name == "" {
		name = "account"
	}
	a := &Account{
		Meta: meta.Metadata{Name: name, DisplayName: orName(src.Meta.DisplayName, src.Meta.Name), Owner: owner},
		Spec: Spec{HostID: src.Spec.HostID, SourceAccountID: src.Meta.ID},
	}
	if r.Kind == ReceiverServiceAccount {
		a.Spec.Holder = &Holder{Kind: HolderServiceAccount, ID: r.ID}
	}
	a.Spec.Kind = a.DerivedKind()
	return a
}

// Caller-account name prefixes, so a team, a project and a user of one name get accounts that read apart.
const (
	teamPrefix     = "team-"
	projectPrefix  = "project-"
	personalPrefix = "user-"
	servicePrefix  = "sa-"
)

// ForTeam is the team's own account, named after it; EnsureManaged mints the final name.
func ForTeam(teamID, teamName, display string) *Account {
	return managed(teamPrefix+teamName, orName(display, teamName), meta.Owner{Kind: meta.OwnerTeam, ID: teamID}, nil)
}

// ForProject is the project's own account.
func ForProject(projectID, projectName, display string) *Account {
	return managed(projectPrefix+projectName, orName(display, projectName), meta.Owner{Kind: meta.OwnerProject, ID: projectID}, nil)
}

// Personal is the user's personal account.
func Personal(userID, username string) *Account {
	return managed(personalPrefix+username, username, meta.Owner{Kind: meta.OwnerUser, ID: userID}, &Holder{Kind: HolderUser, ID: userID})
}

// ForServiceAccount is the service account's account, in its project.
func ForServiceAccount(saID, saName, display, projectID string) *Account {
	return managed(servicePrefix+saName, orName(display, saName), meta.Owner{Kind: meta.OwnerProject, ID: projectID}, &Holder{Kind: HolderServiceAccount, ID: saID})
}

// Member is the user's member account in a team or project scope, named after the user and the scope.
func Member(userID, username string, scope meta.Owner, scopeName, scopeDisplay string) *Account {
	prefix := teamPrefix
	if scope.Kind == meta.OwnerProject {
		prefix = projectPrefix
	}
	return managed(username+"-"+prefix+scopeName, username+" in "+orName(scopeDisplay, scopeName), scope, &Holder{Kind: HolderUser, ID: userID})
}

func managed(base, display string, owner meta.Owner, h *Holder) *Account {
	name := slug.From(base)
	if name == "" {
		name = "account"
	}
	a := &Account{Meta: meta.Metadata{Name: name, DisplayName: display, Owner: owner}, Spec: Spec{Holder: h}}
	a.Spec.Kind = a.DerivedKind()
	return a
}

func orName(display, name string) string {
	if display != "" {
		return display
	}
	return name
}
