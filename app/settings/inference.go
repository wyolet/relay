package settings

// SectionInference is the section key for normal-mode inference behavior
// knobs — settings that affect /v1/* traffic served via an authenticated
// RelayKey. Distinct from proxy-mode (BYO-creds), which has its own
// section.
const SectionInference = "inference"

// Inference controls authenticated-flow behavior.
type Inference struct {
	// AllowMissingPolicy permits a RelayKey with an empty Spec.PolicyID
	// to reach inference endpoints. The request bypasses the per-policy
	// authorization gate: any model served by any host the relay has
	// hostkeys for is reachable, with no policy-level rate limits (system
	// ratelimits still apply). When false, requests from such keys are
	// rejected with 403.
	//
	// Default false. Turn on only for self-hosted setups where the
	// operator is the caller (single-tenant) and wants a god-mode key.
	AllowMissingPolicy bool `json:"allowMissingPolicy"`
}

func (i *Inference) Validate() error { return nil }

func init() {
	Register(Section{
		Name:        SectionInference,
		Description: "Authenticated /v1/* behavior knobs (policy-less keys, etc.)",
		Defaults:    func() any { return &Inference{} },
		Decode:      decodeAndValidate[Inference, *Inference],
	})
}
