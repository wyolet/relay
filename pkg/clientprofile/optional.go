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

	// Catalog metadata a picker can show or size a session against. Zero
	// means the catalog declares nothing, never "zero tokens" — a
	// projection omits the field rather than printing a made-up number.
	ContextWindow   int
	MaxOutputTokens int
	Reasoning       bool
	ToolCall        bool
	Vision          bool
	Temperature     bool
	Modalities      Modalities
	// ReleasedAt is the snapshot's release date as the catalog spells it, "2006-01-02". Empty when the catalog declares none.
	ReleasedAt string
}

// Modalities are the media kinds a model reads and writes, in the catalog's own vocabulary ("text", "image", "audio", "video", "pdf", …). A projection filters to whatever its client's document allows rather than assuming the two vocabularies agree.
type Modalities struct {
	Input  []string
	Output []string
}

// ModelHost is one host serving the entry, with its base-tier rates.
type ModelHost struct {
	Name             string
	Priced           bool // false = no pricing row; do not print $0
	InputUSDPerMtok  float64
	OutputUSDPerMtok float64

	// Cache meters, nil when the rate sheet prices none — nil is "this host publishes no cache rate", never "cached tokens are free".
	CacheReadUSDPerMtok  *float64
	CacheWriteUSDPerMtok *float64

	// Tiers are the above-base rate rows, ascending by threshold.
	Tiers []PriceTier
}

// PriceTier is one above-threshold rate row: what a request pays once its billable input passes AboveTokens. Every meter the host prices is repeated here at its tier value, because a consumer reads a tier as a complete price rather than a patch on the base one. The cache fields follow ModelHost's nil rule.
type PriceTier struct {
	AboveTokens          int
	InputUSDPerMtok      float64
	OutputUSDPerMtok     float64
	CacheReadUSDPerMtok  *float64
	CacheWriteUSDPerMtok *float64
}

// ModelLister is implemented by a profile whose client expects its own
// list-models document.
type ModelLister interface {
	// Models renders the client's list-models document. contentType is
	// what to send back; the app writes body verbatim.
	Models(entries []ModelEntry) (body []byte, contentType string, err error)
}

// ListContext carries what a list projection cannot derive from the entries alone.
type ListContext struct {
	// PublicURL is the externally reachable base of the inference API, without a trailing slash. A document that tells its client which endpoint to call has to print the URL the caller reached relay on, not relay's own listen address.
	PublicURL string
}

// ListerWithContext is ModelLister for a document that must name its own endpoint. The app prefers it over Models when a profile implements both.
type ListerWithContext interface {
	ModelsWithContext(lc ListContext, entries []ModelEntry) (body []byte, contentType string, err error)
}

// ModelListRoute is implemented by a profile whose client reads its model list somewhere other than the default /{profile}/v1/models.
type ModelListRoute interface {
	// ModelListPath is the endpoint's path relative to the profile's /{Name} prefix.
	ModelListPath() string
}

// MultiShape is implemented by a profile whose client speaks more than one inbound wire shape off a single base URL; the app mirrors the profile's prefix over every shape it names. Shape() must return the first of them, so that callers who ask for the primary shape get the same answer either way.
type MultiShape interface {
	Shapes() []string
}

// Speaks reports whether p accepts requests in the named inbound shape, spanning the single- and multi-shape cases so no caller type-asserts MultiShape itself.
func Speaks(p Profile, shape string) bool {
	if p == nil || shape == "" {
		return false
	}
	if ms, ok := p.(MultiShape); ok {
		for _, s := range ms.Shapes() {
			if s == shape {
				return true
			}
		}
		return false
	}
	return p.Shape() == shape
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
