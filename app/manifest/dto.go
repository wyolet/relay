package manifest

import (
	"encoding/json"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

// WireMeta is the metadata block shared by all wire DTOs. ID is optional on
// create (server stamps a UUIDv7); required on update.
type WireMeta struct {
	ID          string            `json:"id,omitempty"          yaml:"id,omitempty"`
	Name        string            `json:"name"                  yaml:"name"`
	DisplayName string            `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Owner       WireOwner         `json:"owner,omitempty"       yaml:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"      yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// WireOwner is the wire form of meta.Owner. The referenced row is named —
// translate functions resolve Name → id when producing the domain shape,
// and reverse-resolve id → Name when emitting the wire shape. ID is the
// id-form for API clients that already hold a UUID; either field is
// accepted on read, with ID taking precedence.
type WireOwner struct {
	Kind meta.OwnerKind `json:"kind,omitempty" yaml:"kind,omitempty" enum:"system,user,team,project,provider,host"`
	Name string         `json:"name,omitempty" yaml:"name,omitempty"`
	ID   string         `json:"id,omitempty"   yaml:"id,omitempty"`
}

// ref returns whichever identifier the caller supplied — id takes
// precedence so API roundtrips that emit id keep working without a name
// resolver. Translate code treats the result as a name-or-id and runs
// the resolver against it.
func (o WireOwner) ref() string {
	if o.ID != "" {
		return o.ID
	}
	return o.Name
}

func (w WireMeta) toMeta() meta.Metadata {
	return meta.Metadata{
		ID:          w.ID,
		Name:        w.Name,
		DisplayName: w.DisplayName,
		Description: w.Description,
		Owner:       meta.Owner{Kind: w.Owner.Kind, ID: w.Owner.ref()},
		Labels:      w.Labels,
		Annotations: w.Annotations,
	}
}

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

// ProviderDTO is the wire form of a Provider. No cross-refs — Provider has
// only display fields in its spec.
type ProviderDTO struct {
	APIVersion string       `json:"apiVersion" yaml:"apiVersion"`
	Kind       string       `json:"kind"       yaml:"kind"`
	Metadata   WireMeta     `json:"metadata"   yaml:"metadata"`
	Spec       ProviderSpec `json:"spec"      yaml:"spec"`
}

type ProviderSpec struct {
	Enabled       *bool      `json:"enabled,omitempty"       yaml:"enabled,omitempty"`
	HomepageURL   string     `json:"homepageURL,omitempty"   yaml:"homepageURL,omitempty"`
	DocsURL       string     `json:"docsURL,omitempty"       yaml:"docsURL,omitempty"`
	StatusPageURL string     `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty"`
	Icon          *meta.Icon `json:"icon,omitempty"          yaml:"icon,omitempty"`
}

// HostDTO is the wire form of a Host.
type HostDTO struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind"       yaml:"kind"`
	Metadata   WireMeta `json:"metadata"   yaml:"metadata"`
	Spec       HostSpec `json:"spec"       yaml:"spec"`
}

type HostSpec struct {
	BaseURL string `json:"baseURL" yaml:"baseURL"`
	// Path overrides the adapter's shape-default upstream path. Verbatim when
	// set; explicit "" appends nothing; absent keeps the shape default.
	Path    *string           `json:"path,omitempty"        yaml:"path,omitempty"`
	Backend map[string]string `json:"backend,omitempty"     yaml:"backend,omitempty"`
	NoAuth  bool              `json:"noAuth,omitempty"      yaml:"noAuth,omitempty"`
	// PricingStrategies is the host's menu of offered billing modes
	// (api | sub). Empty defaults to ["api"].
	PricingStrategies []string   `json:"pricingStrategies,omitempty" yaml:"pricingStrategies,omitempty"`
	Enabled           *bool      `json:"enabled,omitempty"     yaml:"enabled,omitempty"`
	HomepageURL       string     `json:"homepageURL,omitempty" yaml:"homepageURL,omitempty"`
	DocsURL           string     `json:"docsURL,omitempty"     yaml:"docsURL,omitempty"`
	ConsoleURL        string     `json:"consoleURL,omitempty"  yaml:"consoleURL,omitempty"`
	StatusPageURL     string     `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty"`
	Icon              *meta.Icon `json:"icon,omitempty"        yaml:"icon,omitempty"`
}

// ModelDTO is the wire form of a Model.
// Owner.ID in the wire form should be the provider *name*; translate resolves it.
type ModelDTO struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string    `json:"kind"       yaml:"kind"`
	Metadata   WireMeta  `json:"metadata"   yaml:"metadata"`
	Spec       ModelSpec `json:"spec"       yaml:"spec"`
}

type ModelSpec struct {
	Family      string `json:"family,omitempty"      yaml:"family,omitempty"`
	ProductLine string `json:"productLine,omitempty" yaml:"productLine,omitempty"`
	Version     string `json:"version,omitempty"     yaml:"version,omitempty"`

	Capabilities model.Capabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Modalities   model.Modalities   `json:"modalities,omitempty"   yaml:"modalities,omitempty"`

	ContextWindowInput  int `json:"contextWindowInput,omitempty"  yaml:"contextWindowInput,omitempty"`
	ContextWindowOutput int `json:"contextWindowOutput,omitempty" yaml:"contextWindowOutput,omitempty"`
	ContextWindowTotal  int `json:"contextWindowTotal,omitempty"  yaml:"contextWindowTotal,omitempty"`
	MaxOutputTokens     int `json:"maxOutputTokens,omitempty"     yaml:"maxOutputTokens,omitempty"`

	KnowledgeCutoff string             `json:"knowledgeCutoff,omitempty" yaml:"knowledgeCutoff,omitempty"`
	ReleaseDate     string             `json:"releaseDate,omitempty"     yaml:"releaseDate,omitempty"`
	DeprecationDate string             `json:"deprecationDate,omitempty" yaml:"deprecationDate,omitempty"`
	Deprecation     *model.Deprecation `json:"deprecation,omitempty"     yaml:"deprecation,omitempty"`

	Tags                 []string `json:"tags,omitempty"                 yaml:"tags,omitempty"`
	Documentation        string   `json:"documentation,omitempty"        yaml:"documentation,omitempty"`
	License              string   `json:"license,omitempty"              yaml:"license,omitempty"`
	ProviderModelPageURL string   `json:"providerModelPageURL,omitempty" yaml:"providerModelPageURL,omitempty"`

	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	Snapshots []model.Snapshot `json:"snapshots" yaml:"snapshots"`
	Pointer   string           `json:"pointer"   yaml:"pointer"`

	// Aliases are resolution-only matchers (see model.Spec.Aliases). Plain
	// strings, no cross-refs to resolve.
	Aliases []string `json:"aliases,omitempty" yaml:"aliases,omitempty"`
}

// HostBindingDTO is the top-level wire form of a standalone HostBinding entity.
// Model and Host carry *names* (wire form); translate resolves them to ids.
type HostBindingDTO struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string          `json:"kind"       yaml:"kind"`
	Metadata   WireMeta        `json:"metadata"   yaml:"metadata"`
	Spec       HostBindingSpec `json:"spec"       yaml:"spec"`
}

// HostBindingSpec is the spec block of a standalone HostBinding.
type HostBindingSpec struct {
	// Model is the model *name* (wire form).
	Model string `json:"model" yaml:"model"`
	// Host is the host *name* (wire form).
	Host         string `json:"host"                   yaml:"host"`
	Adapter      string `json:"adapter"                yaml:"adapter"`
	UpstreamName string `json:"upstreamName,omitempty" yaml:"upstreamName,omitempty"`
	// Pricing is an optional pricing *name* (wire form).
	Pricing   string   `json:"pricing,omitempty"  yaml:"pricing,omitempty"`
	Enabled   *bool    `json:"enabled,omitempty"  yaml:"enabled,omitempty"`
	Snapshots []string `json:"snapshots,omitempty" yaml:"snapshots,omitempty"`
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

// RateLimitDTO is the wire form of a RateLimit. No cross-refs.
type RateLimitDTO struct {
	APIVersion string        `json:"apiVersion" yaml:"apiVersion"`
	Kind       string        `json:"kind"       yaml:"kind"`
	Metadata   WireMeta      `json:"metadata"   yaml:"metadata"`
	Spec       RateLimitSpec `json:"spec"      yaml:"spec"`
}

type RateLimitSpec struct {
	Rules   []RateLimitRule `json:"rules"             yaml:"rules"`
	Enabled *bool           `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

type RateLimitRule struct {
	Meter    string      `json:"meter"    yaml:"meter"`
	Amount   int64       `json:"amount"   yaml:"amount"`
	Window   interface{} `json:"window"   yaml:"window"` // string ("30s") or int64 ns
	Strategy string      `json:"strategy" yaml:"strategy"`
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

// PricingDTO is the wire form of a Pricing. Owner.ID is a host *name* here.
// TargetModels holds model *names* (wire form).
type PricingDTO struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind"       yaml:"kind"`
	Metadata   WireMeta    `json:"metadata"   yaml:"metadata"`
	Spec       PricingSpec `json:"spec"       yaml:"spec"`
}

type PricingSpec struct {
	Currency     string           `json:"currency"          yaml:"currency"`
	TargetModels []string         `json:"targetModels"      yaml:"targetModels"`
	Rates        []PricingRateDTO `json:"rates"             yaml:"rates"`
	Enabled      *bool            `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// PricingRateDTO mirrors pricing.Rate using plain types.
type PricingRateDTO struct {
	Meter       string  `json:"meter"                 yaml:"meter"`
	Unit        string  `json:"unit"                  yaml:"unit"`
	Amount      float64 `json:"amount"                yaml:"amount"`
	AboveTokens int     `json:"aboveTokens,omitempty" yaml:"aboveTokens,omitempty"`
	ServiceTier string  `json:"serviceTier,omitempty" yaml:"serviceTier,omitempty"`
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
