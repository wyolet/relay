package manifest

import (
	"encoding/json"

	"gopkg.in/yaml.v3"

	catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"
	"github.com/wyolet/relay/pkg/meta"
)

// The catalog kinds' wire DTOs are pkg/catalogkind/manifest's.
type (
	WireMeta        = catalogmanifest.WireMeta
	WireOwner       = catalogmanifest.WireOwner
	ProviderDTO     = catalogmanifest.ProviderDTO
	ProviderSpec    = catalogmanifest.ProviderSpec
	HostDTO         = catalogmanifest.HostDTO
	HostSpec        = catalogmanifest.HostSpec
	ModelDTO        = catalogmanifest.ModelDTO
	ModelSpec       = catalogmanifest.ModelSpec
	HostBindingDTO  = catalogmanifest.HostBindingDTO
	HostBindingSpec = catalogmanifest.HostBindingSpec
	RateLimitDTO    = catalogmanifest.RateLimitDTO
	RateLimitSpec   = catalogmanifest.RateLimitSpec
	RateLimitRule   = catalogmanifest.RateLimitRule
	PricingDTO      = catalogmanifest.PricingDTO
	PricingSpec     = catalogmanifest.PricingSpec
	PricingRateDTO  = catalogmanifest.PricingRateDTO
)

func metaToWire(m meta.Metadata) WireMeta {
	return WireMeta{
		ID:          m.ID,
		Name:        m.Name,
		DisplayName: m.DisplayName,
		Description: m.Description,
		Owner:       WireOwner{Kind: m.Owner.Kind, Name: m.Owner.ID},
		Labels:      m.Labels,
		Annotations: m.Annotations,
	}
}

// HostKeyDTO is the wire form of a HostKey. Spec.HostID is a host *name* here.
type HostKeyDTO struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind"       yaml:"kind"`
	Metadata   WireMeta    `json:"metadata"   yaml:"metadata"`
	Spec       HostKeySpec `json:"spec"       yaml:"spec"`
}

type HostKeySpec struct {
	// HostID carries the host *name* on the wire; translate resolves it to
	// an id when producing the domain shape.
	HostID string `json:"hostId"            yaml:"hostId"`
	// Account is the provider account *name* the key belongs to; keys of one
	// account share its upstream tier limits. Required: declare the Account
	// in the same bundle or name one that exists.
	Account     string           `json:"account,omitempty"     yaml:"account,omitempty"`
	ValueFrom   HostKeyValueFrom `json:"valueFrom"             yaml:"valueFrom"`
	DefaultTier string           `json:"defaultTier,omitempty" yaml:"defaultTier,omitempty"`
	// PricingStrategy is the billing mode this credential is (api | sub).
	// Empty defaults to "api". Must be one of the host's pricingStrategies.
	PricingStrategy string `json:"pricingStrategy,omitempty" yaml:"pricingStrategy,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"     yaml:"enabled,omitempty"`
	Value           string `json:"-"                     yaml:"value,omitempty"`
}

type HostKeyValueFrom struct {
	Kind     string `json:"kind"               yaml:"kind"     enum:"env,stored,aws,azure,gcp,bitwarden,onepassword,oauth"`
	Env      string `json:"env,omitempty"      yaml:"env,omitempty"`
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
}

// KeyDTO is the wire form of a Key. A key holds no policy: its principal is
// bound to one.
type KeyDTO struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind"       yaml:"kind"`
	Metadata   WireMeta `json:"metadata"   yaml:"metadata"`
	Spec       KeySpec  `json:"spec"       yaml:"spec"`
}

type KeySpec struct {
	// Principal names the subject by kind + *name* (wire form): a service
	// account slug or a username.
	Principal PrincipalDTO `json:"principal" yaml:"principal"`
	// Account is the *name* of the key's default lane; omitted takes the principal's own account.
	Account               string  `json:"account,omitempty"           yaml:"account,omitempty"`
	KeyHash               string  `json:"keyHash"                     yaml:"keyHash"`
	Prefix                string  `json:"prefix,omitempty"            yaml:"prefix,omitempty"`
	ExpiresAt             *string `json:"expiresAt,omitempty"         yaml:"expiresAt,omitempty"`
	RevokedAt             *string `json:"revokedAt,omitempty"         yaml:"revokedAt,omitempty"`
	Enabled               *bool   `json:"enabled,omitempty"           yaml:"enabled,omitempty"`
	PassthroughAllowed    bool    `json:"passthroughAllowed,omitempty" yaml:"passthroughAllowed,omitempty"`
	PayloadLoggingEnabled bool    `json:"payloadLoggingEnabled,omitempty" yaml:"payloadLoggingEnabled,omitempty"`
}

// PrincipalDTO is the wire form of a Key principal.
type PrincipalDTO struct {
	Kind string `json:"kind" yaml:"kind" enum:"serviceaccount,user"`
	Name string `json:"name" yaml:"name"`
}

// TeamDTO is the wire form of a Team. No cross-refs.
type TeamDTO struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind"       yaml:"kind"`
	Metadata   WireMeta `json:"metadata"   yaml:"metadata"`
	Spec       TeamSpec `json:"spec"       yaml:"spec"`
}

type TeamSpec struct {
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// ProjectDTO is the wire form of a Project. Spec.Team is a team *name*.
type ProjectDTO struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind"       yaml:"kind"`
	Metadata   WireMeta    `json:"metadata"   yaml:"metadata"`
	Spec       ProjectSpec `json:"spec"       yaml:"spec"`
}

type ProjectSpec struct {
	// Team is the owning team *name* (wire form).
	Team    string `json:"team"              yaml:"team"`
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// SettingDTO is the wire form of a settings section. Unlike the catalog kinds
// the spec shape is not fixed — it varies per section, with metadata.name
// selecting the registered settings.Section whose typed value the spec must
// match. The spec is therefore carried as a raw node and validated downstream
// by that section's Decode. Settings are singletons keyed by name, so the
// owner/id/label metadata fields are unused.
type SettingDTO struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string    `json:"kind"       yaml:"kind"`
	Metadata   WireMeta  `json:"metadata"   yaml:"metadata"`
	Spec       yaml.Node `json:"-"          yaml:"spec"`
}

// SpecJSON renders the raw spec node as JSON for the settings store, which
// validates it against the section's typed value via the section's Decode.
func (d *SettingDTO) SpecJSON() (json.RawMessage, error) {
	var v any
	if err := d.Spec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// ServiceAccountDTO is the wire form of a ServiceAccount. Spec.Project is
// a project *name*, Spec.Policy a policy *name*.
type ServiceAccountDTO struct {
	APIVersion string             `json:"apiVersion" yaml:"apiVersion"`
	Kind       string             `json:"kind"       yaml:"kind"`
	Metadata   WireMeta           `json:"metadata"   yaml:"metadata"`
	Spec       ServiceAccountSpec `json:"spec"       yaml:"spec"`
}

type ServiceAccountSpec struct {
	Project string `json:"project"           yaml:"project"`
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// GroupDTO is the wire form of a Group. Spec.Members holds *usernames*.
type GroupDTO struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string    `json:"kind"       yaml:"kind"`
	Metadata   WireMeta  `json:"metadata"   yaml:"metadata"`
	Spec       GroupSpec `json:"spec"       yaml:"spec"`
}

type GroupSpec struct {
	Members []string `json:"members,omitempty" yaml:"members,omitempty"`
	Enabled *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// RoleDTO is the wire form of a Role. Rules name API plurals and verbs
// verbatim — there is nothing to resolve.
type RoleDTO struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind"       yaml:"kind"`
	Metadata   WireMeta `json:"metadata"   yaml:"metadata"`
	Spec       RoleSpec `json:"spec"       yaml:"spec"`
}

type RoleSpec struct {
	Rules   []RoleRuleDTO `json:"rules"             yaml:"rules"             minItems:"1"`
	Enabled *bool         `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// RoleRuleDTO mirrors role.Rule using plain types.
type RoleRuleDTO struct {
	Kinds []string `json:"kinds" yaml:"kinds" minItems:"1" enum:"*,accounts,audit,budgets,debug,groups,host-bindings,host-keys,hosts,keys,license,logs,master-key,models,policies,policy-bindings,pools,pricings,projects,providers,rate-limits,role-bindings,roles,scim-clients,secret-keys,service-accounts,settings,system,teams,tokens,usage,users"`
	Verbs []string `json:"verbs" yaml:"verbs" minItems:"1" enum:"*,apply,attach,create,delete,destroy,detach,generate,get,health,list,mint,read,reload,revoke,rewrap,rotate,snapshot,update"`
}

// SubjectDTO is the wire form of a binding subject: everything is named, so
// a user carries a username, a service account its slug, and a group the
// group name (local or IdP).
type SubjectDTO struct {
	Kind string `json:"kind" yaml:"kind" enum:"user,serviceaccount,group"`
	Name string `json:"name" yaml:"name"`
}

// RoleBindingDTO is the wire form of a RoleBinding. Spec.Role is a role
// *name*, Spec.Scope names a team or project, and subjects are named.
type RoleBindingDTO struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string          `json:"kind"       yaml:"kind"`
	Metadata   WireMeta        `json:"metadata"   yaml:"metadata"`
	Spec       RoleBindingSpec `json:"spec"       yaml:"spec"`
}

type RoleBindingSpec struct {
	Role string `json:"role" yaml:"role"`
	// Scope is a system, team, or project reference in the same shape every
	// owner uses; the system scope carries no name.
	Scope    WireOwner    `json:"scope"             yaml:"scope"`
	Subjects []SubjectDTO `json:"subjects"          yaml:"subjects" minItems:"1"`
	Enabled  *bool        `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// PolicyBindingDTO is the wire form of a PolicyBinding. Spec.Policy and
// Spec.Accounts are *names*; metadata.owner names the system, team, project
// or user owning it.
type PolicyBindingDTO struct {
	APIVersion string            `json:"apiVersion" yaml:"apiVersion"`
	Kind       string            `json:"kind"       yaml:"kind"`
	Metadata   WireMeta          `json:"metadata"   yaml:"metadata"`
	Spec       PolicyBindingSpec `json:"spec"       yaml:"spec"`
}

type PolicyBindingSpec struct {
	Policy   string   `json:"policy"            yaml:"policy"`
	Accounts []string `json:"accounts"          yaml:"accounts" minItems:"1"`
	Enabled  *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}
