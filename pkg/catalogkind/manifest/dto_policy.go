package manifest

// PolicyDTO is the wire form of a Policy: a rule set naming the models,
// pools, rate limits and budgets requests under it may use. Every link is a
// *name*. A catalog tier policy links models and rate limits only.
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
	// names; see pkg/modelref. Empty allows every model the pools serve.
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
