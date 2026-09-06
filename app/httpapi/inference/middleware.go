package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/metrics"
)

// ctxKeyT is the context-value key used to stash the authenticated
// Key for handlers to read via KeyFromContext.
type ctxKeyT struct{}

// ctxPrincipalT is the context-value key for the resolved Principal.
type ctxPrincipalT struct{}

// ctxSnapshotT is the context-value key for the snapshot the credential was
// resolved against.
type ctxSnapshotT struct{}

// WithSnapshot pins snap as the catalog view for everything downstream of
// ctx. The credential middleware and the WebSocket per-frame path are the
// only writers.
func WithSnapshot(ctx context.Context, snap *appcatalog.Snapshot) context.Context {
	return context.WithValue(ctx, ctxSnapshotT{}, snap)
}

// SnapshotFrom returns the catalog view this request was authenticated
// against, or nil when no credential middleware ran (anonymous proxy).
// Downstream phases read it rather than cat.Current() so a reload landing
// mid-request cannot split one request across two snapshots.
func SnapshotFrom(ctx context.Context) *appcatalog.Snapshot {
	if v, ok := ctx.Value(ctxSnapshotT{}).(*appcatalog.Snapshot); ok {
		return v
	}
	return nil
}

// Credential kinds a Principal can be authenticated by.
const (
	CredentialKey   = "key"
	CredentialToken = "token"
)

// Principal is who the request is acting as, resolved once at the edge so
// nothing downstream re-derives it. Credential-specific fields
// (CredentialKind, CredentialID) name the credential presented, not the
// subject.
type Principal struct {
	Subjects                     []string
	UserID, ServiceAccountID     string // exactly one set
	ProjectID, TeamID            string // empty for personal keys
	CredentialKind, CredentialID string // CredentialKey/CredentialToken; key id or jti
	KeyHash                      string // hash presented; empty for tokens
	Key                          *key.Key
	ServiceAccount               *serviceaccount.ServiceAccount
	Policy                       *policy.Policy // nil → policy-less
	PassthroughAllowed           bool
	PayloadLogging               bool

	// TokenExp/TokenVer are the claims a token credential was admitted
	// under, kept so a long-lived connection can re-check them without the
	// bearer. Zero for a key.
	TokenExp int64
	TokenVer int
}

// Recheck re-validates an already-admitted principal against the current
// snapshot. The HTTP path resolves once per request and never needs it; a
// WebSocket admits on the upgrade and then serves frames for hours, so
// every frame re-runs the revocation checks the upgrade made. Snapshot
// reads only — no signature work, no Postgres.
func (p *Principal) Recheck(snap *appcatalog.Snapshot, now time.Time) error {
	if p == nil || snap == nil {
		return nil
	}
	// A credential scoped to a project stops working when the project (or
	// the team above it) leaves the snapshot: its limits and attribution
	// no longer exist.
	if p.ProjectID != "" {
		if _, ok := snap.Project(p.ProjectID); !ok {
			return errors.New("project unavailable")
		}
	}
	if p.CredentialKind == CredentialToken {
		if p.TokenExp > 0 && p.TokenExp <= now.Unix() {
			return errors.New("token expired")
		}
		if ver, ok := snap.TokenVersion(p.UserID); !ok || ver != p.TokenVer {
			return errors.New(msgTokenRevoked)
		}
		return nil
	}
	k, matchedPrevious := snap.KeyByHash(p.KeyHash)
	switch {
	case k == nil:
		return errors.New("invalid api key")
	case !k.IsEnabled():
		return errors.New("api key disabled")
	case k.Spec.RevokedAt != nil:
		return errors.New("api key revoked")
	case k.Spec.ExpiresAt != nil && !now.Before(*k.Spec.ExpiresAt):
		return errors.New("api key expired")
	case matchedPrevious && !k.InGrace(now):
		return errors.New("api key rotated")
	}
	return nil
}

// PolicyID returns the resolved policy's id, or "" for the policy-less
// flow. Nil-safe so error paths can call it unguarded.
func (p *Principal) PolicyID() string {
	if p == nil || p.Policy == nil {
		return ""
	}
	return p.Policy.Meta.ID
}

// reserveIdentity returns what the inbound reservation is scoped by: the
// caller's team (the kv hash tag) and, for a token, the jti whose denylist
// entry rides the same script.
func reserveIdentity(ctx context.Context) (teamID, tokenJTI string) {
	p := PrincipalFrom(ctx)
	if p == nil {
		return "", ""
	}
	if p.CredentialKind == CredentialToken {
		return p.TeamID, p.CredentialID
	}
	return p.TeamID, ""
}

// KeyFromContext returns the authenticated relay key from ctx, or
// nil if no key middleware fired.
func KeyFromContext(ctx context.Context) *key.Key {
	if v, ok := ctx.Value(ctxKeyT{}).(*key.Key); ok {
		return v
	}
	return nil
}

// PrincipalFrom returns the resolved principal from ctx, or nil when the
// request carried no credential (anonymous proxy mode).
func PrincipalFrom(ctx context.Context) *Principal {
	if v, ok := ctx.Value(ctxPrincipalT{}).(*Principal); ok {
		return v
	}
	return nil
}

// PrincipalMiddleware authenticates the inbound credential according to the
// request's Mode classification (set by ClassifyMiddleware upstream) and
// resolves who it acts as:
//
//   - ModeNormal       — credential is required; lookup must succeed.
//   - ModeProxyAuthed  — credential is required; lookup must succeed and the
//     principal must be allowed to bring its own upstream key.
//   - ModeProxyAnonymous — no credential; this middleware is a no-op and
//     neither a *Key nor a *Principal is stashed on ctx.
//
// The bearer is either a key or a relay-minted token, told apart by shape
// (looksLikeToken); both resolve to the same Principal and only the lookup
// differs. Every read is against the in-memory snapshot, re-read per request
// so admin edits take effect within the NOTIFY debounce window.
func PrincipalMiddleware(cat *appcatalog.Catalog, tokens *TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cls := ClassificationFrom(r.Context())
			if cls.Mode == ModeProxyAnonymous {
				// Gate (Settings.ProxyMode.AllowUnauthenticated) is checked
				// downstream in the handler; this middleware just doesn't
				// require a key.
				next.ServeHTTP(w, r)
				return
			}
			if cls.Key == "" {
				writeAuthErr(w, "missing relay key")
				return
			}
			snap := cat.Current()
			var (
				p  *Principal
				k  *key.Key
				ok bool
			)
			if looksLikeToken(cls.Key) {
				if p, ok = tokenPrincipal(w, snap, tokens, cls.Key); !ok {
					return
				}
			} else if p, k, ok = keyPrincipal(w, snap, cls.Key); !ok {
				return
			}
			if !resolvePolicy(w, snap, p) {
				return
			}
			if cls.Mode == ModeProxyAuthed && !p.PassthroughAllowed {
				writeForbidden(w, "passthrough_forbidden", "this credential may not forward upstream keys")
				return
			}
			ctx := WithSnapshot(r.Context(), snap)
			if k != nil {
				ctx = context.WithValue(ctx, ctxKeyT{}, k)
			}
			ctx = context.WithValue(ctx, ctxPrincipalT{}, p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// keyPrincipal authenticates a key bearer, writing the 401 and reporting
// false when the key is unknown or no longer usable.
func keyPrincipal(w http.ResponseWriter, snap *appcatalog.Snapshot, bearer string) (*Principal, *key.Key, bool) {
	hash := hashToken(bearer)
	k, matchedPrevious := snap.KeyByHash(hash)
	if k == nil {
		writeAuthErr(w, "invalid api key")
		return nil, nil, false
	}
	now := time.Now()
	switch {
	case !k.IsEnabled():
		writeAuthErr(w, "api key disabled")
		return nil, nil, false
	case k.Spec.RevokedAt != nil:
		writeAuthErr(w, "api key revoked")
		return nil, nil, false
	case k.Spec.ExpiresAt != nil && !now.Before(*k.Spec.ExpiresAt):
		writeAuthErr(w, "api key expired")
		return nil, nil, false
	case matchedPrevious && !k.InGrace(now):
		writeAuthErr(w, "api key rotated")
		return nil, nil, false
	}
	return buildPrincipal(snap, k, hash), k, true
}

// buildPrincipal resolves the key's identity and tenancy from the snapshot.
// Subjects is the precomputed list, taken by slice header — never rebuilt
// per request.
func buildPrincipal(snap *appcatalog.Snapshot, k *key.Key, hash string) *Principal {
	p := &Principal{
		Subjects:           snap.SubjectsForKey(k.Meta.ID),
		CredentialKind:     CredentialKey,
		CredentialID:       k.Meta.ID,
		KeyHash:            hash,
		Key:                k,
		PassthroughAllowed: k.Spec.PassthroughAllowed,
		PayloadLogging:     k.Spec.PayloadLoggingEnabled,
	}
	if k.Spec.PolicyID != "" {
		if pol, ok := policyOrDisabled(snap, k.Spec.PolicyID); ok {
			p.Policy = pol
		}
	}
	switch k.Spec.Principal.Kind {
	case key.PrincipalServiceAccount:
		p.ServiceAccountID = k.Spec.Principal.ID
		if sa, ok := snap.ServiceAccount(k.Spec.Principal.ID); ok {
			p.ServiceAccount = sa
			p.ProjectID = sa.Spec.ProjectID
			if proj, ok := snap.Project(sa.Spec.ProjectID); ok {
				p.TeamID = proj.Spec.TeamID
			}
		}
	case key.PrincipalUser:
		p.UserID = k.Spec.Principal.ID
		// A personal key on a project-owned policy spends that project's
		// upstream credentials (its owner was allowed to point it there), so
		// the request carries the project's attribution and limits.
		if p.Policy != nil && p.Policy.Meta.Owner.Kind == meta.OwnerProject {
			p.ProjectID = p.Policy.Meta.Owner.ID
			if proj, ok := snap.Project(p.ProjectID); ok {
				p.TeamID = proj.Spec.TeamID
			}
		}
	}
	return p
}

// resolvePolicy fills in the Policy a principal doesn't already carry from
// its credential: the service account's override first, then the project's
// policy bindings in (priority, name) order. Reports false — after writing
// the response — when nothing resolves and the caller is not eligible for
// the policy-less flow.
func resolvePolicy(w http.ResponseWriter, snap *appcatalog.Snapshot, p *Principal) bool {
	if p.Policy == nil && p.ServiceAccount != nil && p.ServiceAccount.Spec.PolicyID != "" {
		if pol, ok := policyOrDisabled(snap, p.ServiceAccount.Spec.PolicyID); ok {
			p.Policy = pol
		}
	}
	if p.Policy == nil && p.ProjectID != "" {
		for _, b := range snap.PolicyBindingsForProject(p.ProjectID) {
			if !bindingMatches(b, p.Subjects) {
				continue
			}
			if pol, ok := policyOrDisabled(snap, b.Spec.PolicyID); ok {
				p.Policy = pol
				break
			}
		}
	}
	if p.Policy != nil {
		// A resolved-but-disabled policy is an answer, not a miss: falling
		// through to a broader binding, or to the policy-less flow, would
		// hand the caller more than the operator left switched on.
		if !p.Policy.IsEnabled() {
			writeForbidden(w, "policy_disabled", "policy is disabled")
			return false
		}
		return true
	}
	// A personal key with no project falls through to the policy-less flow,
	// which routing gates on settings.Inference.AllowMissingPolicy. Anything
	// scoped to a project — a token always is — must resolve a policy.
	if p.CredentialKind == CredentialKey && p.ProjectID == "" {
		return true
	}
	writeForbidden(w, "no_policy", "no policy is bound to this principal")
	return false
}

// policyOrDisabled resolves a policy id, falling back to the disabled row so
// a credential pointing at a switched-off policy is answered rather than
// treated as pointing at nothing (D77).
func policyOrDisabled(snap *appcatalog.Snapshot, id string) (*policy.Policy, bool) {
	if pol, ok := snap.Policy(id); ok {
		return pol, true
	}
	return snap.DisabledPolicy(id)
}

// bindingMatches reports whether the binding names a subject the principal
// carries. Both lists hold a handful of entries, so the nested scan beats
// building a set per request.
func bindingMatches(b *policybinding.PolicyBinding, subjects []string) bool {
	for _, want := range b.SubjectKeys {
		for _, have := range subjects {
			if have == want {
				return true
			}
		}
	}
	return false
}

func bearer(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return h[len(prefix):]
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// authRejectedTotal answers "are callers being turned away at the edge, and
// why" without one log line per rejection: an unauthenticated flood would
// otherwise amplify into the log pipeline.
var authRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "inference",
	Name:      "auth_rejected_total",
	Help:      "Inference requests refused by the credential middleware, by reason.",
}, []string{"reason"})

func init() { metrics.Register(authRejectedTotal) }

// writeForbidden reports a caller that authenticated but may not proceed.
func writeForbidden(w http.ResponseWriter, code, msg string) {
	authRejectedTotal.WithLabelValues(code).Inc()
	slog.Debug("inference: auth rejected", "status", 403, "code", code, "msg", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"` + code + `","message":"` + msg + `"}}`))
}

// writeAuthErr rejects an unauthenticated caller. msg doubles as the metric
// reason: every call site passes one of a fixed set of literals.
func writeAuthErr(w http.ResponseWriter, msg string) {
	// Revocation answers one code whichever path caught it — the version
	// bump here, the jti denylist inside the reservation.
	code := "unauthenticated"
	if msg == msgTokenRevoked {
		code = "token_revoked"
	}
	authRejectedTotal.WithLabelValues(msg).Inc()
	slog.Debug("inference: auth rejected", "status", 401, "code", code, "msg", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"` + code + `","message":"` + msg + `"}}`))
}

// msgTokenRevoked is the message the token-version check answers with; the
// WebSocket recheck reuses it, so both report the same code.
const msgTokenRevoked = "token revoked"
