package settings

// SectionOTLPReceiver is the section key for the OpenTelemetry receiver on the inference listener.
const SectionOTLPReceiver = "otlp-receiver"

// OTLPReceiver configures the endpoints that accept telemetry clients report about model calls relay did not carry.
type OTLPReceiver struct {
	// Enabled accepts exports at POST /otlp/v1/traces and /otlp/v1/logs. Off by default: reported usage is self-declared by the client and is recorded, never enforced.
	Enabled bool `json:"enabled"`
	// CaptureContent stores the message content a client reports with a call (instructions, input and output messages, tool definitions) in the payload store. It takes effect only while payload logging is enabled. Off by default: content is the client's data and the largest part of an export.
	CaptureContent bool `json:"captureContent,omitempty"`
}

// Validate is enforced before any write.
func (o *OTLPReceiver) Validate() error { return nil }

func init() {
	Register(Section{
		Name:        SectionOTLPReceiver,
		Description: "OpenTelemetry receiver. When enabled, the inference listener accepts OTLP/HTTP exports at /otlp/v1/traces and /otlp/v1/logs from callers holding a relay key or token, and records the model calls they describe as usage events with source \"otlp\". Reported usage is not subject to policies or to the rate limits of inference; export requests are capped per credential (1,200 per minute unless a rate limit named otlp-export says otherwise). captureContent also stores the message content clients report, in the payload store, while payload logging is enabled. Both default off. Hot-reloaded.",
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
