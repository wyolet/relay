// Package audit records who did what on the control plane: every mutating
// admin action, every denial, and every login/logout, as one row per HTTP
// request.
//
// Capture happens at three seams rather than per handler, so a new admin
// endpoint is covered the moment it calls Authorize:
//
//   - Middleware mints an in-flight Event on the request context (actor,
//     request id, method, path, client IP) and emits it after the response
//     with the observed HTTP status.
//   - Authorizer wraps the configured authz.Authorizer: each Authorize call
//     contributes an action, a resource (with its scope chain) and an
//     allow/deny status; the marking rule then decides whether the request
//     earns a row.
//   - Changed and Record are the two explicit calls: Changed attaches the
//     JSON paths a write touched; Record covers handlers (login, logout)
//     that never call Authorize.
//
// Values are never recorded — only field paths, and a path naming a secret
// is dropped outright. Writes go through a bounded, drop-on-full Emitter so
// no handler ever waits on the audit store. Reading rows back is the
// control plane's GET /api/audit; retention is the "audit" settings
// section, applied by the emitter's hourly prune.
//
// Out of scope: data-plane traffic (usage events already record it) and
// export to an external SIEM.
package audit
