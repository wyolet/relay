package manifest

// PolicyDTO is the wire form of a Policy: a rule set naming the models,
// pools, rate limits and budgets requests under it may use. Every link is a
// *name*.
type PolicyDTO struct {
	APIVersion string     `json:"apiVersion" yaml:"apiVersion"`
	Kind       string     `json:"kind"       yaml:"kind"`
	Metadata   WireMeta   `json:"metadata"   yaml:"metadata"`
	Spec       PolicySpec `json:"spec"       yaml:"spec"`
}

type PolicySpec struct {
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// Pools are pool names, tried in order: a later pool is the failover for
	// an earlier one. Host-owned (tier) policies list none.
	Pools []string `json:"pools,omitempty" yaml:"pools,omitempty"`
	// Models holds model refs ("provider", "provider/model"), not bare model
	// names; see app/modelref. Empty allows every model the pools serve.
	Models     []string             `json:"models,omitempty"     yaml:"models,omitempty"`
	RateLimits []PolicyRateLimitDTO `json:"rateLimits,omitempty" yaml:"rateLimits,omitempty"`
	Budgets    []PolicyBudgetDTO    `json:"budgets,omitempty"    yaml:"budgets,omitempty"`
}

// PolicyRateLimitDTO links one rate limit by name: to the whole policy when
// Models is empty, otherwise only to requests for a matching model. Shared
// omitted means true.
type PolicyRateLimitDTO struct {
	Ref    string   `json:"ref"              yaml:"ref"`
	Models []string `json:"models,omitempty" yaml:"models,omitempty"`
	Shared *bool    `json:"shared,omitempty" yaml:"shared,omitempty"`
}

// PolicyBudgetDTO links one budget by name. Shared omitted means true.
type PolicyBudgetDTO struct {
	Ref    string `json:"ref"              yaml:"ref"`
	Shared *bool  `json:"shared,omitempty" yaml:"shared,omitempty"`
}

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
	Host  string `json:"host,omitempty"  yaml:"host,omitempty"`
	OrgID string `json:"orgId,omitempty" yaml:"orgId,omitempty"`
	// Default marks the account its owner's keys at the host join when they name none; at most one per owner and host.
	Default        bool   `json:"default,omitempty"        yaml:"default,omitempty"`
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
