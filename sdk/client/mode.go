package client

import (
	"context"
	"fmt"
	"os"

	"github.com/wyolet/relay/sdk/telemetry"
)

// Mode is where a model call goes and who records it.
type Mode string

const (
	// ModeProxy sends calls through relay (WR_BASE_URL, WR_API_KEY), which records them.
	ModeProxy Mode = "proxy"
	// ModeTelemetry sends calls to the provider with the caller's provider key and reports each one to relay's OTLP receiver.
	ModeTelemetry Mode = "telemetry"
	// ModeDirect sends calls to the provider; nothing is reported.
	ModeDirect Mode = "direct"
)

// EnvMode selects the Mode of ForMode when no WithMode option is given.
const EnvMode = "WR_MODE"

// ModeOption configures ForMode.
type ModeOption func(*modeConfig)

type modeConfig struct {
	mode        Mode
	providerKey string
	emitter     *telemetry.Emitter
	clientOpts  []Option
}

// WithMode picks the mode, overriding WR_MODE.
func WithMode(m Mode) ModeOption { return func(c *modeConfig) { c.mode = m } }

// WithProviderKey sets the provider key of the telemetry and direct modes, overriding the vendor's key env var (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY then GOOGLE_API_KEY, by the resolved binding's adapter).
func WithProviderKey(key string) ModeOption { return func(c *modeConfig) { c.providerKey = key } }

// WithEmitter sets the emitter of the telemetry mode. Without it ForMode builds one from the environment, which the client's Close closes. The other modes ignore it.
func WithEmitter(e *telemetry.Emitter) ModeOption { return func(c *modeConfig) { c.emitter = e } }

// WithClientOptions applies client options (an HTTP client, headers, ...) to the client of any mode.
func WithClientOptions(opts ...Option) ModeOption {
	return func(c *modeConfig) { c.clientOpts = append(c.clientOpts, opts...) }
}

// ForMode builds a client for the model ref in one of three modes. The mode is WithMode, else WR_MODE, else proxy when WR_BASE_URL and WR_API_KEY are set, else direct; telemetry is never implied. Proxy sends every call through relay with ref as its model. Telemetry and direct resolve ref against the embedded catalog like For and call the provider; telemetry also reports each call (to WR_BASE_URL + "/otlp" with WR_API_KEY, or the OTEL_EXPORTER_OTLP_* settings, unless WithEmitter is given).
func ForMode(ref string, opts ...ModeOption) (*Client, error) {
	var cfg modeConfig
	for _, o := range opts {
		o(&cfg)
	}
	mode := cfg.mode
	if mode == "" {
		mode = Mode(os.Getenv(EnvMode))
	}
	if mode == "" {
		mode = ModeDirect
		if os.Getenv(EnvBaseURL) != "" && os.Getenv(EnvAPIKey) != "" {
			mode = ModeProxy
		}
	}

	switch mode {
	case ModeProxy:
		c := Relay("", "", cfg.clientOpts...)
		c.target.upstream = ref
		return c, nil
	case ModeDirect, ModeTelemetry:
	default:
		return nil, fmt.Errorf("relay client: invalid mode %q (want %s, %s or %s)", mode, ModeProxy, ModeTelemetry, ModeDirect)
	}

	clientOpts := cfg.clientOpts
	var owned *telemetry.Emitter
	if mode == ModeTelemetry {
		e := cfg.emitter
		if e == nil {
			e = telemetry.New()
			owned = e
		}
		clientOpts = append(clientOpts[:len(clientOpts):len(clientOpts)], WithTelemetry(e))
	}
	c, err := For(ref, cfg.providerKey, WithClient(clientOpts...))
	if err != nil {
		if owned != nil {
			_ = owned.Close(context.Background())
		}
		return nil, err
	}
	c.ownsTelemetry = owned != nil
	if c.apiKey == "" {
		for _, name := range c.target.adapter.keyEnv {
			if c.apiKey = os.Getenv(name); c.apiKey != "" {
				break
			}
		}
	}
	return c, nil
}
