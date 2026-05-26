package settings

// SectionParsing is the section key for request-parsing behavior knobs —
// how much structure the relay extracts from inbound request bodies
// before dispatch. Applies across flows (authenticated /v1/* and
// proxy-mode), so it is its own section rather than living under
// inference or proxy-mode.
const SectionParsing = "parsing"

// Parsing controls inbound request-body parsing depth.
type Parsing struct {
	// RichParsing extracts per-request metadata and messages from the
	// body (for attribution / observability). When false the relay
	// reads only the minimal fields needed to route (model, stream),
	// leaving metadata and messages unparsed — lower CPU on the hot
	// path, no body-level observability.
	//
	// Default true. Hot-swappable: a change takes effect on the next
	// request within a reconcile interval, no restart.
	RichParsing bool `json:"richParsing"`
}

func (p *Parsing) Validate() error { return nil }

func init() {
	Register(Section{
		Name:        SectionParsing,
		Description: "Inbound request-body parsing depth. RichParsing extracts per-request metadata + messages for attribution/observability; off reads only routing fields (model, stream). Default on. Hot-reloaded.",
		Defaults:    func() any { return &Parsing{RichParsing: true} },
		Decode:      decodeAndValidate[Parsing, *Parsing],
	})
}
