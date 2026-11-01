package settings

// SectionProxyMode is the section key for the proxy-mode settings — the
// singleton that gates anonymous-key (BYO upstream creds) inference
// traffic served by the future app/proxy flow.
const SectionProxyMode = "proxy-mode"

// ProxyMode controls whether the relay accepts inference requests where
// the caller brings their own upstream provider key instead of a relay
// key. The flow lives in app/proxy; the inference credential middleware
// enforces this section before the request body is read.
type ProxyMode struct {
	// Enabled turns the proxy flow on. When false, requests without a
	// valid relay key get 401, regardless of any other field below.
	Enabled bool `json:"enabled" doc:"Accept requests that carry the caller's own upstream credential; while off they are refused with 403 proxy_disabled."`

	// AllowUnauthenticated lets proxy callers reach the upstream with
	// no relay-side auth — the inbound Authorization header is forwarded
	// as-is. When false, the proxy flow still requires a relay key
	// (proxied traffic gets logged against that key) but the upstream
	// credential comes from the caller.
	AllowUnauthenticated bool `json:"allowUnauthenticated" doc:"Let proxy callers through without a relay key, forwarding their Authorization header as is. False still requires a relay key, with the upstream credential from the caller."`
}

func (p *ProxyMode) Validate() error { return nil }

func init() {
	Register(Section{
		Name:     SectionProxyMode,
		Defaults: func() any { return &ProxyMode{} },
		Decode:   decodeAndValidate[ProxyMode, *ProxyMode],
	})
}
