package settings

// SectionProxyMode is the section key for the proxy-mode settings — the
// singleton that gates anonymous-key (BYO upstream creds) inference
// traffic served by the future app/proxy flow.
const SectionProxyMode = "proxy-mode"

// ProxyMode controls whether the relay accepts inference requests where
// the caller brings their own upstream provider key instead of a relay
// key. The flow lives in app/proxy (not yet built); this section gates
// the dispatch at the inference handler.
type ProxyMode struct {
	// Enabled turns the proxy flow on. When false, requests without a
	// valid relay key get 401, regardless of any other field below.
	Enabled bool `json:"enabled"`

	// AllowedHostSlugs restricts which Host rows the proxy flow may
	// forward to. Empty = no restriction (any enabled Host). Names
	// are validated by reference at admin-write time but the snapshot
	// re-checks at request time in case a host was disabled.
	AllowedHostSlugs []string `json:"allowedHostSlugs,omitempty"`
}

// Validate enforces invariants on the proxy-mode section. Currently
// trivial — kept as a method so future fields (request quotas, allowed
// origins, etc.) have a single home.
func (p *ProxyMode) Validate() error {
	// Dedup AllowedHostSlugs without sorting (operator order is
	// preserved for display); empty entries get dropped.
	seen := make(map[string]struct{}, len(p.AllowedHostSlugs))
	out := p.AllowedHostSlugs[:0]
	for _, s := range p.AllowedHostSlugs {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	p.AllowedHostSlugs = out
	return nil
}

func init() {
	Register(Section{
		Name:     SectionProxyMode,
		Defaults: func() any { return &ProxyMode{} },
		Decode:   decodeAndValidate[ProxyMode, *ProxyMode],
	})
}
