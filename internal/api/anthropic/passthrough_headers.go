package anthropic

import (
	"net/http"

	"github.com/wyolet/relay/pkg/httpheader"
)

// passthroughHeaders are forwarded verbatim from inbound to upstream when a
// policy is in passthrough mode. They carry Anthropic/Claude-Code client-identity
// signals that upstream inspects (OAuth beta gating, billing UA, session id).
var passthroughHeaders = []string{
	"Anthropic-Beta",
	"Anthropic-Version",
	"Anthropic-Dangerous-Direct-Browser-Access",
	"User-Agent",
	"X-App",
	"X-Claude-Code-Session-Id",
	"X-Stainless-*",
}

func capturePassthroughHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	for name, vals := range h {
		if httpheader.Match(name, passthroughHeaders) && len(vals) > 0 {
			out[name] = vals[0]
		}
	}
	return out
}
