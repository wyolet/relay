package manifest

// PolicyDTO is the wire form of a Policy: the gate linking a subject to the
// models, pools, credentials, rate limits and budgets it may use. Every link
// is a *name*.
type PolicyDTO struct {
	APIVersion string     `json:"apiVersion" yaml:"apiVersion"`
	Kind       string     `json:"kind"       yaml:"kind"`
	Metadata   WireMeta   `json:"metadata"   yaml:"metadata"`
	Spec       PolicySpec `json:"spec"       yaml:"spec"`
}

type PolicySpec struct {
	Enabled           *bool `json:"enabled,omitempty"           yaml:"enabled,omitempty"`
	IncludeDeprecated bool  `json:"includeDeprecated,omitempty" yaml:"includeDeprecated,omitempty"`
	// Pools are pool names, tried in order: a later pool is the failover for
	// an earlier one. Host-owned (tier) policies list none.
	Pools []string `json:"pools,omitempty" yaml:"pools,omitempty"`
	// Credentials are host-key names linked directly, each a pool of one.
	// Host-owned (tier) policies list none.
	Credentials []string `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// Models holds modelref DSL strings ("provider", "provider/model",
	// "@host", ...), not bare model names; see app/modelref.
	Models     []string             `json:"models,omitempty"     yaml:"models,omitempty"`
	RateLimits []PolicyRateLimitDTO `json:"rateLimits,omitempty" yaml:"rateLimits,omitempty"`
	// Budgets are budget names.
	Budgets               []string `json:"budgets,omitempty"               yaml:"budgets,omitempty"`
	PayloadLoggingEnabled bool     `json:"payloadLoggingEnabled,omitempty" yaml:"payloadLoggingEnabled,omitempty"`
}

// PolicyRateLimitDTO links one rate limit by name: to the whole policy when
// Models is empty, otherwise only to requests for a matching model.
type PolicyRateLimitDTO struct {
	Ref    string   `json:"ref"              yaml:"ref"`
	Models []string `json:"models,omitempty" yaml:"models,omitempty"`
}

// AccountDTO is the wire form of an Account: one provider organization at
// one host. Spec.Host is a host *name*.
type AccountDTO struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind"       yaml:"kind"`
	Metadata   WireMeta    `json:"metadata"   yaml:"metadata"`
	Spec       AccountSpec `json:"spec"       yaml:"spec"`
}

type AccountSpec struct {
	Host  string `json:"host"            yaml:"host"`
	OrgID string `json:"orgId,omitempty" yaml:"orgId,omitempty"`
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
