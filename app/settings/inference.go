package settings

// SectionInference is the section key for normal-mode inference behavior
// knobs — settings that affect /v1/* traffic served via an authenticated
// Key. Distinct from proxy-mode (BYO-creds), which has its own
// section.
const SectionInference = "inference"

// Inference controls authenticated-flow behavior.
type Inference struct {
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
		Description: "Authenticated /v1/* behavior knobs (key rotation grace)",
		Defaults:    func() any { return &Inference{} },
		Decode:      decodeAndValidate[Inference, *Inference],
	})
}
