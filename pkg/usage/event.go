// Package usage defines the per-request usage Event, the Sink and Reader interfaces its storage backends implement, and the in-memory filtering and aggregation shared by scan-style backends.
package usage

import (
	"time"

	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// Event is the canonical per-request usage record: attribution, token
// counts, and the cost stamped once at emit time. Cost is a historical
// fact computed from the pricing in effect when the request ran — never
// recomputed at read time (pricing is mutable config). This package only
// carries the stamped fields; pricing resolution and the cost math live
// app-side (app/usagelog + app/pricing).
//
// It lives in pkg/usage (not app/) so every sink/reader backend
// (file, ClickHouse, valkey, postgres) can consume it as a vendorable
// type without importing app/ — preserving the pkg-purity rule. The
// app/usagelog package re-exports it (type alias) for its lifecycle
// observers.
//
// Field order is fixed (Go marshals in struct order) so JSONL / jq /
// ClickHouse column mapping stays stable across releases.
type Event struct {
	// Identity / trace
	RequestID string    `json:"request_id" doc:"Request id."`
	Source    string    `json:"source"     enum:"pipeline,proxy,ws,batch,count,otlp" doc:"Which path served the request: pipeline (relay-routed), proxy (caller's own upstream credential), ws (WebSocket), batch, count (token counting), or otlp (reported by a client)."` // "pipeline" | "proxy" | "ws" | "batch" | "count" | "otlp"
	Timestamp time.Time `json:"ts"         doc:"When the request started."`

	// Outcome
	Status       int    `json:"status"                  doc:"HTTP status the caller received: the provider's, or relay's own when relay refused the request (refusal set). 0 for a request relay failed before any upstream answered for another reason."`
	DurationMs   int64  `json:"duration_ms"             doc:"Milliseconds from request start until the response closed."` // total: start → response closed
	Streamed     bool   `json:"streamed,omitempty"      doc:"The response was streamed."`
	FinishReason string `json:"finish_reason,omitempty" doc:"Why generation stopped: stop, length, tool_calls, content_filter, refusal or other."` // "stop"|"length"|"tool_calls"|"content_filter"|"refusal"|"other"
	Attempts     int    `json:"attempts,omitempty"      doc:"Upstream attempts, counting failover; 0 when not tracked."`                           // upstream tries (pipeline failover); 0 = not tracked
	ErrorKind    string `json:"error_kind,omitempty"    doc:"Error code of a failed request, such as model_not_allowed, rate_limit_exceeded or upstream_error."`
	Refusal      string `json:"refusal,omitempty"       enum:"model_not_allowed,host_not_reachable,no_capacity" doc:"Set when relay refused the request itself, as the class of error_kind: model_not_allowed (a policy, account or principal refuses the model), host_not_reachable (no host the caller can reach serves it, or none of them accepted a connection), no_capacity (a rate limit, budget, cooldown, or every host key out of rotation). Absent on success and on every other failure, an upstream's own error included."`
	ErrorMessage string `json:"error_message,omitempty" doc:"Error message of a failed request."`

	// Upstream is the upstream-leg timing breakdown. Nil when the request
	// never reached upstream (routing/pre-flight failure). The total
	// (start → close) is DurationMs; this adds the finer marks DurationMs
	// can't express. See UpstreamTiming for unit + how to derive TTFT.
	Upstream *sdkusage.UpstreamTiming `json:"upstream,omitempty" doc:"Upstream timing marks; absent when the request never reached an upstream."`

	// Reasoning is the reasoning span (when the model emitted reasoning
	// content). Nil unless the response was a canonical-observed stream
	// that carried reasoning. Microseconds from start, like Upstream.
	Reasoning *sdkusage.ReasoningTiming `json:"reasoning,omitempty" doc:"The reasoning span, for a translated stream that carried reasoning."`

	// Attribution — UUIDs (stable, snapshot-resolvable to slugs at
	// query time). Hash of the inbound bearer is included so the
	// plaintext is never logged.
	RelayKeyHash   string `json:"relay_key_hash,omitempty"  doc:"Hash of the relay key presented; empty for tokens and anonymous proxy traffic."`
	PolicyID       string `json:"policy_id,omitempty"       doc:"Id of the innermost caller policy that governed the request."`
	ModelID        string `json:"model_id,omitempty"        doc:"Id of the resolved model."`
	RequestedModel string `json:"requested_model,omitempty" doc:"The model string as the caller sent it."` // model string as the caller sent it
	HostID         string `json:"host_id,omitempty"         doc:"Id of the host that served the request."`
	// HostKeyID is the hash of the upstream credential's value (the key the
	// keypool breaker is keyed by), not the HostKey row id: a rotated key
	// records a new value.
	HostKeyID string `json:"host_key_id,omitempty" doc:"Hash of the upstream credential's value (not the host key's row id), so a rotated key records a new value."`
	// PoolID and AccountID are the pool the serving credential was drawn from and the provider account it belongs to; empty when it was linked on its own or holds no account. PolicyID is the innermost caller policy.
	PoolID    string `json:"pool_id,omitempty"    doc:"Id of the pool the serving host key was drawn from; empty when the policy linked the key directly."`
	AccountID string `json:"account_id,omitempty" doc:"Id of the provider account the serving host key belongs to."`

	// Tenancy + principal attribution as resolved at request entry.
	// Project/Team/Principal are the event-time slugs beside their ids —
	// same denormalization rationale as Model/Host/Policy below. Principal
	// is empty for a user principal (users live in Postgres, not the
	// snapshot the entry reads). CredentialKind/CredentialID name the
	// credential presented, not the subject; RelayKeyHash stays the hash of
	// the inbound bearer and is empty for token credentials.
	ProjectID      string `json:"project_id,omitempty"      doc:"Id of the caller's project."`
	Project        string `json:"project,omitempty"         doc:"The project's slug at request time."`
	TeamID         string `json:"team_id,omitempty"         doc:"Id of the caller's team."`
	Team           string `json:"team,omitempty"            doc:"The team's slug at request time."`
	PrincipalKind  string `json:"principal_kind,omitempty"  enum:"serviceaccount,user" doc:"Kind of the principal the request acted as."`
	PrincipalID    string `json:"principal_id,omitempty"    doc:"Id of the principal."`
	Principal      string `json:"principal,omitempty"       doc:"The service account's slug at request time; empty for a user."`
	CredentialKind string `json:"credential_kind,omitempty" doc:"Kind of credential presented, such as key or token."`
	CredentialID   string `json:"credential_id,omitempty"   doc:"Id of the credential presented: the relay key's id or the token's jti."`
	LaneAccountID  string `json:"lane_account_id,omitempty" doc:"Id of the account the request ran in and was charged to: the credential's default, or the one the request switched into. Not account_id, the serving host key's provider account."`

	// Token usage as reported by the upstream. Empty on error or when
	// the adapter could not extract.
	Tokens sdkusage.Tokens `json:"tokens,omitempty" doc:"Token counts by kind (input, output, cache_read, reasoning, …) as the upstream reported them."`

	// Free-form per-runner tags (client_ip for anonymous proxy, etc.)
	Extras map[string]string `json:"extras,omitempty" doc:"Relay-recorded details, such as client_ip for anonymous proxy traffic."`

	// Tags are caller-owned (X-WR-Request-Tags, validated post-flight);
	// Extras stays relay-stamped — the provenance split queries rely on.
	Tags map[string]string `json:"tags,omitempty" doc:"Tags the caller sent in X-WR-Request-Tags."`

	// Model, Host, Policy are the entities' slugs (metadata.name) at
	// event time, denormalized so events stay self-describing when the
	// catalog entity is gone (deleted, renamed, re-seeded). The *_id
	// fields above remain the precise keys.
	Model  string `json:"model,omitempty"  doc:"The model's slug at request time."`
	Host   string `json:"host,omitempty"   doc:"The host's slug at request time."`
	Policy string `json:"policy,omitempty" doc:"The policy's slug at request time."`

	// Provider is the model's owning provider slug at event time — same
	// denormalization rationale as Model/Host/Policy above.
	Provider string `json:"provider,omitempty" doc:"The model provider's slug at request time."`

	// RelayKey is the inbound key's slug at event time, beside RelayKeyHash
	// and CredentialID — same denormalization rationale as Model/Host/Policy
	// above. Empty when no key was presented (token, anonymous proxy).
	RelayKey string `json:"relay_key,omitempty" doc:"The relay key's slug at request time; empty when no key was presented."`

	// Cost, stamped once at emit time from the pricing then in effect.
	// CostNanos is integer nano-USD (1 USD = 1e9) — integer so sums never
	// drift; USD-only by decision, so no currency field. nil = unpriced
	// (no pricing resolved, or no priceable tokens — error/LogOnly rows),
	// which MUST stay distinguishable from a true $0: never collapse nil
	// to 0. CostBreakdown is per-meter nanos (keys like "tokens.input");
	// Pricing is the rate sheet's slug, stamped whenever one resolved
	// (even if the row ended up unpriced) for audit.
	CostNanos     *int64           `json:"cost_nanos,omitempty"     doc:"Cost in nano-USD (1 USD = 1e9), computed from the pricing in effect at request time. Absent means unpriced, which is not the same as 0."`
	CostBreakdown map[string]int64 `json:"cost_breakdown,omitempty" doc:"Cost in nano-USD per meter, such as tokens.input."`
	Pricing       string           `json:"pricing,omitempty"        doc:"Slug of the pricing sheet that applied."`
}

// ErrorKindUpstream is the kind of an event whose provider answered with an error status (Status >= 400), whether relay passed the response through, gave up after trying every key, or was told about the call by a client.
const ErrorKindUpstream = "upstream_error"

// LogOnly reports whether the event records a request rejected before any
// upstream was reached: a relay refusal (which carries the status relay
// answered), or status 0 with an ErrorKind set. Such events stay visible in
// event listings (the logs view) but are excluded from Summary and
// TimeSeries aggregation — they carry no usage and would otherwise pollute
// stats as phantom unattributed requests.
func (e Event) LogOnly() bool { return e.Refusal != "" || (e.Status == 0 && e.ErrorKind != "") }

// Aggregated reports whether an aggregate counts e: every row but a log-only one, and a log-only relay refusal too when refusals is set.
func (e Event) Aggregated(refusals bool) bool { return !e.LogOnly() || (refusals && e.Refusal != "") }

// CountsRefused reports whether an aggregate over q grouped by groupBy counts relay's own refusals: only one that filters or groups by refusal, so every other aggregate keeps its served-request numbers.
func CountsRefused(q EventQuery, groupBy string) bool {
	return len(q.Refusal) > 0 || groupBy == "refusal"
}
