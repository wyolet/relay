// Package clientprofile identifies which coding agent (Claude Code, Codex,
// OpenCode, …) is calling the relay, so the edge can serve each one the
// model-list shape, error wording and attribution it expects.
//
// Scope: identification only — a Profile is data plus (later) serializers
// over neutral value types. Deliberately out of scope: any per-client
// branching inside app/ (the core resolves a Profile and asks it), and any
// knowledge of routing, the catalog or the pipeline. The package imports
// nothing from app/ or internal/.
package clientprofile

import "net/http"

// Profile identifies a client and the inbound wire shape it speaks.
// Later stages add optional interfaces (model-list projection, error
// envelope, keepalive, attribution); this one stays small.
type Profile interface {
	// Name is the URL prefix segment and the X-WR-Client value,
	// e.g. "claude-code". Empty for Default.
	Name() string

	// Shape is the inbound adapter name whose routes are mirrored under
	// /{Name}/, e.g. "anthropic". Empty for Default.
	Shape() string

	// Match sniffs the request (User-Agent, marker headers) when neither
	// the header nor the path prefix named a profile.
	Match(r *http.Request) bool
}

// Default is the profile for callers that name no client: today's
// behaviour, byte-identical. Resolve returns it when nothing matches.
var Default Profile = defaultProfile{}

type defaultProfile struct{}

func (defaultProfile) Name() string             { return "" }
func (defaultProfile) Shape() string            { return "" }
func (defaultProfile) Match(*http.Request) bool { return false }
