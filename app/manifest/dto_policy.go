package manifest

import catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"

// Policy is a catalog kind; its wire DTOs are pkg/catalogkind/manifest's.
type (
	PolicyDTO          = catalogmanifest.PolicyDTO
	PolicySpec         = catalogmanifest.PolicySpec
	PolicyRateLimitDTO = catalogmanifest.PolicyRateLimitDTO
	PolicyBudgetDTO    = catalogmanifest.PolicyBudgetDTO
)

// AccountDTO is the wire form of an Account. A provider account names its
// host by *name*; a caller account (team, project, personal, member,
// service) carries no host and is made by relay, so a manifest may only
// update one that exists.
type AccountDTO struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind"       yaml:"kind"`
	Metadata   WireMeta    `json:"metadata"   yaml:"metadata"`
	Spec       AccountSpec `json:"spec"       yaml:"spec"`
}

type AccountSpec struct {
	Host           string `json:"host,omitempty"  yaml:"host,omitempty"`
	OrgID          string `json:"orgId,omitempty" yaml:"orgId,omitempty"`
	PayloadLogging string `json:"payloadLogging,omitempty" yaml:"payloadLogging,omitempty" enum:"on,off"`
	// SourceAccount names the real account a shared account reaches; host names the same host. Fixed once created.
	SourceAccount string `json:"sourceAccount,omitempty" yaml:"sourceAccount,omitempty"`
	// ServiceAccount names the service account a share with one is held by; metadata.owner is its project.
	ServiceAccount string `json:"serviceAccount,omitempty" yaml:"serviceAccount,omitempty"`
}

// PoolDTO is the wire form of a Pool: one host's keys and how a key is
// picked. Spec.Host and Spec.Credentials are *names*.
type PoolDTO struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind"       yaml:"kind"`
	Metadata   WireMeta `json:"metadata"   yaml:"metadata"`
	Spec       PoolSpec `json:"spec"       yaml:"spec"`
}

type PoolSpec struct {
	Host string `json:"host" yaml:"host"`
	// Accounts are provider-account names whose keys the pool draws on whole, tried before Credentials.
	Accounts []string `json:"accounts,omitempty" yaml:"accounts,omitempty"`
	// Credentials are host-key names; order is the preference for
	// prioritized selection.
	Credentials []string `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	Selection   string   `json:"selection,omitempty"   yaml:"selection,omitempty" enum:"prioritized,round-robin,least-recently-used"`
}

// BudgetDTO is the wire form of a Budget: a recurring USD spend cap.
type BudgetDTO struct {
	APIVersion string     `json:"apiVersion" yaml:"apiVersion"`
	Kind       string     `json:"kind"       yaml:"kind"`
	Metadata   WireMeta   `json:"metadata"   yaml:"metadata"`
	Spec       BudgetSpec `json:"spec"       yaml:"spec"`
}

type BudgetSpec struct {
	// AmountUSD is a decimal with at most two fractional digits.
	AmountUSD string `json:"amountUsd"        yaml:"amountUsd"`
	Period    string `json:"period,omitempty" yaml:"period,omitempty" enum:"day,week,month"`
	Mode      string `json:"mode,omitempty"   yaml:"mode,omitempty"   enum:"hard,alert"`
}
