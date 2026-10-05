package settings

// SectionOTLPReceiver is the section key for the OpenTelemetry receiver on the inference listener.
const SectionOTLPReceiver = "otlp-receiver"

// OTLPReceiver configures the endpoint that accepts telemetry clients report about model calls relay did not carry.
type OTLPReceiver struct {
	// Enabled accepts trace exports at POST /otlp/v1/traces. Off by default: reported usage is self-declared by the client and is recorded, never enforced.
	Enabled bool `json:"enabled"`
}

// Validate is enforced before any write.
func (o *OTLPReceiver) Validate() error { return nil }

func init() {
	Register(Section{
		Name:        SectionOTLPReceiver,
		Description: "OpenTelemetry receiver. When enabled, the inference listener accepts OTLP/HTTP trace exports at /otlp/v1/traces from callers holding a relay key or token, and records the model calls they describe as usage events with source \"otlp\". Reported usage is not subject to policies or to the rate limits of inference; export requests are capped per credential by the otlp-export system rate limit. Default off. Hot-reloaded.",
		Defaults:    func() any { return &OTLPReceiver{} },
		Decode:      decodeAndValidate[OTLPReceiver, *OTLPReceiver],
	})
}

// OTLPReceiverFrom reads the typed section from a settings Reader, tolerating absent or mistyped values (returns the zero value: disabled).
func OTLPReceiverFrom(r Reader) *OTLPReceiver {
	if r == nil {
		return &OTLPReceiver{}
	}
	if v, ok := r.Setting(SectionOTLPReceiver); ok {
		if c, ok := v.(*OTLPReceiver); ok {
			return c
		}
	}
	return &OTLPReceiver{}
}
