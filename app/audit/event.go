package audit

import (
	"time"

	"github.com/wyolet/relay/app/meta"
)

// Actor kinds. Anonymous covers unauthenticated callers — notably a failed
// login, where the attempted username is all that is known. A SCIM client is
// an identity provider provisioning through /scim/v2, named by its client name.
// CLI is a relay command run against the database directly, named by the OS user.
const (
	ActorUser       = "user"
	ActorAdminToken = "admin-token"
	ActorAnonymous  = "anonymous"
	ActorSCIMClient = "scim-client"
	ActorCLI        = "cli"
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
	Kind      string `json:"kind"                enum:"user,admin-token,anonymous,scim-client,cli" doc:"Who acted: a signed-in user, the admin token, an unauthenticated caller (such as a failed login), a SCIM client, or a relay CLI command run against the database."`
	ID        string `json:"id,omitempty"        doc:"User id, when a user acted."`
	Name      string `json:"name,omitempty"      doc:"Username, attempted username, SCIM client name or OS user."`
	SessionID string `json:"sessionId,omitempty" doc:"Session the request came on, for a signed-in user."`
	IP        string `json:"ip,omitempty"        doc:"Client address, honouring trusted proxies."`
}

// Resource is the target of the action, as it was passed to Authorize.
// Scope is the owner's scope chain rendered as "project:<id>" / "team:<id>",
// most specific first; the global scope is omitted.
type Resource struct {
	Kind  string      `json:"kind"            doc:"Target kind, as an API singular."`
	ID    string      `json:"id,omitempty"    doc:"Target id."`
	Name  string      `json:"name,omitempty"  doc:"Target slug."`
	Owner *meta.Owner `json:"owner,omitempty" doc:"The target's owner."`
	Scope []string    `json:"scope,omitempty" doc:"The owner's scope chain as project:<id> and team:<id>, most specific first; the system scope is left out."`
}

// Outcome is the authorization verdict plus the HTTP status the caller saw.
type Outcome struct {
	Status string `json:"status" enum:"allowed,denied,error" doc:"The authorization verdict; error is an authorizer failure, not a denial."`
	Code   int    `json:"code"   doc:"HTTP status the caller received."`
}

// Request identifies the HTTP request the event came from.
type Request struct {
	ID     string `json:"id,omitempty"     doc:"Request id."`
	Method string `json:"method,omitempty" doc:"HTTP method."`
	Path   string `json:"path,omitempty"   doc:"Request path."`
}

// Change lists the JSON paths a write touched. Values are never carried —
// the paths plus the row's own history answer "what changed".
type Change struct {
	Fields []string `json:"fields,omitempty" doc:"JSON paths the write changed; values are never recorded."`
}

// Event is one audited control-plane request.
type Event struct {
	ID       string    `json:"id"               doc:"Event id."`
	TS       time.Time `json:"ts"               doc:"When the request was handled."`
	Actor    Actor     `json:"actor"            doc:"Who made the request."`
	Action   string    `json:"action"           doc:"The authorized action, such as policies.update."`
	Resource Resource  `json:"resource"         doc:"What the action targeted."`
	Outcome  Outcome   `json:"outcome"          doc:"The verdict and the status returned."`
	Request  Request   `json:"request"          doc:"The HTTP request."`
	Change   *Change   `json:"change,omitempty" doc:"What a write changed; absent for reads and refusals."`
}
