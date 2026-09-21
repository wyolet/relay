package clientprofile

import "net/http"

// The interfaces below are optional: the app type-asserts a Profile for
// each and skips the seam when it is not implemented. Default implements
// none of them.

// ModelEntry is the neutral view of one catalog model the app hands to a
// profile for its list projection. No catalog types cross into pkg/.
type ModelEntry struct {
	ID          string // catalog model slug, what the client sends as `model`
	Model       string // parent model slug; the prefix the ID's snapshot suffix hangs off
	DisplayName string
	Pointer     bool     // the snapshot the parent model points at — the "latest" one
	Aliases     []string // resolution-only aliases that also route to this model
	Hosts       []ModelHost
}

// ModelHost is one host serving the entry, with its base-tier rates.
type ModelHost struct {
	Name             string
	Priced           bool // false = no pricing row; do not print $0
	InputUSDPerMtok  float64
	OutputUSDPerMtok float64
}

// ModelLister is implemented by a profile whose client expects its own
// list-models document.
type ModelLister interface {
	// Models renders the client's list-models document. contentType is
	// what to send back; the app writes body verbatim.
	Models(entries []ModelEntry) (body []byte, contentType string, err error)
}

// Route is one extra endpoint a profile serves. Path is relative to the
// profile's /{Name} prefix.
type Route struct {
	Method  string
	Path    string
	Handler http.Handler

	// Public mounts the route without the relay-key auth chain — for
	// endpoints a client probes before it has necessarily attached
	// credentials. Such handlers must stay cheap and say nothing about
	// the deployment.
	Public bool
}

// Router is implemented by a profile needing endpoints beyond the wire
// shape's own inbound routes (connection probes and the like).
type Router interface {
	Routes() []Route
}

// ModelNamer is implemented by a profile whose client needs model ids reshaped for its picker; the app applies Inbound to the `model` field of every request that resolved to this profile, before routing.
type ModelNamer interface {
	// Inbound maps a picker id back to a catalog model reference, and is the identity for anything this profile did not mint.
	Inbound(model string) string
}

// Attributor is implemented by a profile whose client sends identifying
// request headers worth recording against the request's usage.
type Attributor interface {
	// AttributionHeaders lists request header names to capture into the
	// request's usage metadata, keyed by the lowercased header name.
	AttributionHeaders() []string
}

// TokenCountRoute is implemented by a profile whose client asks the gateway to count a prompt's input tokens before sending it.
type TokenCountRoute interface {
	// TokenCountPath is the endpoint's path relative to the profile's /{Name} prefix. The app mounts POST there behind the inference auth chain and answers with the count for the request body.
	TokenCountPath() string
}

// SessionKeyer is implemented by a profile whose client marks the requests of one conversation, so relay can keep per-conversation observations without the app knowing which header carries the mark.
type SessionKeyer interface {
	// SessionKey returns the conversation identifier h carries, or "" when the client sent none.
	SessionKey(h http.Header) string
}
