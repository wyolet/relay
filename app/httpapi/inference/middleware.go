package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/serviceaccount"
)

// ctxKeyT is the context-value key used to stash the authenticated
// Key for handlers to read via KeyFromContext.
type ctxKeyT struct{}

// ctxPrincipalT is the context-value key for the resolved Principal.
type ctxPrincipalT struct{}

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
			ctx := r.Context()
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
		if pol, ok := snap.Policy(k.Spec.PolicyID); ok {
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
		if pol, ok := snap.Policy(p.ServiceAccount.Spec.PolicyID); ok {
			p.Policy = pol
		}
	}
	if p.Policy == nil && p.ProjectID != "" {
		for _, b := range snap.PolicyBindingsForProject(p.ProjectID) {
			if !bindingMatches(b, p.Subjects) {
				continue
			}
			if pol, ok := snap.Policy(b.Spec.PolicyID); ok {
				p.Policy = pol
				break
			}
		}
	}
	if p.Policy != nil {
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

// bindingMatches reports whether the binding names a subject the principal
// carries. Both lists hold a handful of entries, so the nested scan beats
// building a set per request.
func bindingMatches(b *policybinding.PolicyBinding, subjects []string) bool {
	for i := range b.Spec.Subjects {
		want := b.Spec.Subjects[i].Key()
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

// writeForbidden reports a caller that authenticated but may not proceed.
func writeForbidden(w http.ResponseWriter, code, msg string) {
	slog.Warn("inference: auth rejected", "status", 403, "code", code, "msg", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"` + code + `","message":"` + msg + `"}}`))
}

func writeAuthErr(w http.ResponseWriter, msg string) {
	slog.Warn("inference: auth rejected", "status", 401, "code", "unauthenticated", "msg", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"unauthenticated","message":"` + msg + `"}}`))
}
