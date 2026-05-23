package lifecycle

import (
	"time"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

// Context is the persistent lifecycle state for one request. Created
// once at request entry, threaded through every phase, mutable by
// middleware to enrich routing identity and free-form metadata.
//
// Fields fall into three layers:
//
//   - Identity: set at entry, never changes after.
//   - Routing identity: filled during routing / pre-flight middleware
//     once the (model, host, binding, keys) tuple is resolved.
//   - Metadata: free-form cross-hook channel for facts that don't
//     deserve first-class fields. Middlewares scribble; observers read.
//
// Middlewares may mutate any field; observers must treat fields as
// read-only (see package doc — concurrent map writes on Metadata are a
// race). A field being empty means "not yet known" or "not applicable
// for this runner" (e.g. HostKeyID stays empty in proxy mode where the
// caller brought their own credential).
type Context struct {
	// Identity (immutable after construction).

	RequestID string
	Source    string // runner label: "pipeline" | "proxy" | "ws" | "batch"
	StartTime time.Time

	// Routing identity (filled during routing / pre-flight).

	RelayKeyHash string
	PolicyID     string
	ModelID      string
	HostID       string
	HostKeyID    string

	// Cross-hook channel. Middleware writes; observers read.
	// Concurrent map writes during post-flight are a panic — keep
	// writes to the pre-flight phase only, or wrap with your own lock
	// if you really must.
	Metadata map[string]any

	// Translator is the per-request vendor adapter, set by the runner
	// when routing decides the upstream. Observers that want a
	// canonical view of the response (usage, finish reason, output
	// items) call v1.ExtractUsage / Translator.ParseResponse on
	// ev.ResponseBody. nil for runners that can't expose one (e.g.
	// anonymous proxy without resolved binding).
	Translator v1.Translator
}

// NewContext returns a Context with required identity fields set and a
// fresh Metadata map. The runner fills routing identity later, as
// routing progresses.
func NewContext(requestID, source string, startTime time.Time) *Context {
	return &Context{
		RequestID: requestID,
		Source:    source,
		StartTime: startTime,
		Metadata:  map[string]any{},
	}
}
