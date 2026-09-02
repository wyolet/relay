package settings

// SectionInference is the section key for normal-mode inference behavior
// knobs — settings that affect /v1/* traffic served via an authenticated
// Key. Distinct from proxy-mode (BYO-creds), which has its own
// section.
const SectionInference = "inference"

// Inference controls authenticated-flow behavior.
type Inference struct {
	// AllowMissingPolicy permits a Key with an empty Spec.PolicyID
	// to reach inference endpoints. The request bypasses the per-policy
	// authorization gate: any model served by any host the relay has
	// hostkeys for is reachable, with no policy-level rate limits (system
	// ratelimits still apply). When false, requests from such keys are
	// rejected with 403.
	//
	// Read only under single-user authorization (RELAY_AUTHZ=single).
	// Under rbac a credential's grants are the whole access model, so a
	// key with no policy is rejected whatever this says.
	//
	// Default false. Turn on only for self-hosted setups where the
	// operator is the caller (single-tenant) and wants a god-mode key.
	AllowMissingPolicy bool `json:"allowMissingPolicy"`

	// KeyRotateMaxGrace caps, in seconds, how long a rotated key's previous
	// hash keeps authenticating. Zero means the default below; rotation
	// requests above the cap are rejected.
	KeyRotateMaxGrace int `json:"keyRotateMaxGrace"`
}

// DefaultKeyRotateMaxGrace is the cap applied when the section leaves
// KeyRotateMaxGrace unset.
const DefaultKeyRotateMaxGrace = 24 * 60 * 60

// MaxRotateGrace resolves the configured cap, falling back to the default.
func (i *Inference) MaxRotateGrace() int {
	if i.KeyRotateMaxGrace <= 0 {
		return DefaultKeyRotateMaxGrace
	}
	return i.KeyRotateMaxGrace
}

func (i *Inference) Validate() error { return nil }

func init() {
	Register(Section{
		Name:        SectionInference,
		Description: "Authenticated /v1/* behavior knobs (policy-less keys, key rotation grace, etc.)",
		Defaults:    func() any { return &Inference{} },
		Decode:      decodeAndValidate[Inference, *Inference],
	})
}
