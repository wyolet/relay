package audit

import (
	"time"

	"github.com/wyolet/relay/app/meta"
)

// Actor kinds. Anonymous covers unauthenticated callers — notably a failed
// login, where the attempted username is all that is known.
const (
	ActorUser       = "user"
	ActorAdminToken = "admin-token"
	ActorAnonymous  = "anonymous"
)

// Outcome statuses. Error is an authorizer backend failure, which is not
// the same as a denial.
const (
	StatusAllowed = "allowed"
	StatusDenied  = "denied"
	StatusError   = "error"
)

// Actor is the caller an event is attributed to.
type Actor struct {
	Kind      string `json:"kind"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	IP        string `json:"ip,omitempty"`
}

// Resource is the target of the action, as it was passed to Authorize.
// Scope is the owner's scope chain rendered as "project:<id>" / "team:<id>",
// most specific first; the global scope is omitted.
type Resource struct {
	Kind  string      `json:"kind"`
	ID    string      `json:"id,omitempty"`
	Name  string      `json:"name,omitempty"`
	Owner *meta.Owner `json:"owner,omitempty"`
	Scope []string    `json:"scope,omitempty"`
}

// Outcome is the authorization verdict plus the HTTP status the caller saw.
type Outcome struct {
	Status string `json:"status"`
	Code   int    `json:"code"`
}

// Request identifies the HTTP request the event came from.
type Request struct {
	ID     string `json:"id,omitempty"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Change lists the JSON paths a write touched. Values are never carried —
// the paths plus the row's own history answer "what changed".
type Change struct {
	Fields []string `json:"fields,omitempty"`
}

// Event is one audited control-plane request.
type Event struct {
	ID       string    `json:"id"`
	TS       time.Time `json:"ts"`
	Actor    Actor     `json:"actor"`
	Action   string    `json:"action"`
	Resource Resource  `json:"resource"`
	Outcome  Outcome   `json:"outcome"`
	Request  Request   `json:"request"`
	Change   *Change   `json:"change,omitempty"`
}
