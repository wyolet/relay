package lifecycle

import (
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
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

	// Timing carries the per-request checkpoints. Timing.Start is the
	// absolute anchor (set at construction); the runner stamps the
	// upstream + end marks as the request progresses. See timing.go.
	Timing Timing

	// EventTime is a caller-asserted event timestamp (X-WR-Event-Time,
	// honored only under the dev trust flag). Zero = unset. It overrides
	// the usage Event's Timestamp only — never Timing.Start, which every
	// duration derives from and must stay real.
	EventTime time.Time

	// Streamed reports whether the response was streamed back to the
	// caller. Set by the runner once known (request flag in pipeline,
	// upstream Content-Type in proxy).
	Streamed bool

	// RequestedModel is the model identifier the caller asked for, as it
	// arrived on the wire — before resolution to the catalog Model id
	// (ModelID). Set at the inference entry.
	RequestedModel string

	// Attempts is the number of upstream tries the pipeline made (1 when
	// the first key succeeded; >1 on failover). Pipeline-only; stays 0 in
	// proxy mode, which is single-shot by design.
	Attempts int

	// Routing identity (filled during routing / pre-flight).

	RelayKeyHash string
	PolicyID     string
	ModelID      string
	HostID       string
	HostKeyID    string

	// PayloadLog opts this request into full request/response body capture
	// by the payloadlog observer. Set at the inference entry from the
	// routing Plan (Policy or RelayKey opt-in). When false, the payload
	// observer skips the request and its stream observer does not buffer.
	PayloadLog bool

	// RequestBody is the raw inbound request bytes, retained for the
	// payloadlog observer. Set at the inference entry (pipeline: the
	// dispatched body; proxy: a capped tee of the request stream). Nil
	// when payload logging is off or the body wasn't retained. It is a
	// reference to the dispatch buffer, not a copy — never mutated.
	RequestBody []byte

	// RequestBodyTruncated marks RequestBody as an incomplete prefix of a
	// larger streamed body — set on the proxy peek-then-stream path when
	// the remainder wasn't captured (payload logging off, the tee capture
	// hit its cap, or upstream answered before draining the request) — so
	// payload records never pretend the stored bytes were the whole
	// request. False whenever RequestBody holds the complete body.
	RequestBodyTruncated bool

	// Cross-hook channel. Middleware writes; observers read.
	// Concurrent map writes during post-flight are a panic — keep
	// writes to the pre-flight phase only, or wrap with your own lock
	// if you really must.
	Metadata map[string]any

	// collected holds each Hook's filled result keyed by the hook's
	// Name(). Written ONLY by the Registry (attach), serially — hooks
	// never touch it, so it can't race or be left inconsistent. Read by
	// Collectors (store) and by pre-send readers (e.g. usage echo).
	collected map[string]any

	// filled records that the Registry already ran the hooks for this
	// request. Set by Registry.Fill so a pre-send fill (echo) isn't
	// repeated by the post-send Finalize. Registry-only.
	filled bool

	// Translator is the per-request vendor adapter, set by the runner
	// when routing decides the upstream. Observers that want a
	// canonical view of the response (usage, finish reason, output
	// items) call v1.ExtractUsage / Translator.ParseResponse on
	// ev.ResponseBody. nil for runners that can't expose one (e.g.
	// anonymous proxy without resolved binding).
	Translator v1.Translator
}

// NewContext returns a Context with required identity fields set, the
// timing anchor stamped, and a fresh Metadata map. The runner fills
// routing identity and the remaining timing marks later, as the request
// progresses.
func NewContext(requestID, source string, startTime time.Time) *Context {
	return &Context{
		RequestID: requestID,
		Source:    source,
		Timing:    Timing{Start: startTime},
		Metadata:  map[string]any{},
		collected: map[string]any{},
	}
}

// attach records a Hook's filled result under name. Unexported on
// purpose: only the Registry (same package) writes the collected set,
// and only serially — that's what guarantees hooks can't race it.
func (c *Context) attach(name string, v any) {
	if c == nil || v == nil {
		return
	}
	if c.collected == nil {
		c.collected = map[string]any{}
	}
	c.collected[name] = v
}

// Collected returns the result a Hook attached under name, or (nil,
// false) if none. Read-only access for Collectors (store side) and
// pre-send readers (e.g. usage echo). Nil-safe.
func (c *Context) Collected(name string) (any, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.collected[name]
	return v, ok
}
