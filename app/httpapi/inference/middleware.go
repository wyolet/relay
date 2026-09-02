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
)

// ctxKeyT is the context-value key used to stash the authenticated
// Key for handlers to read via KeyFromContext.
type ctxKeyT struct{}

// ctxPrincipalT is the context-value key for the resolved Principal.
type ctxPrincipalT struct{}

// Principal is who the request is acting as, resolved once at the edge so
// nothing downstream re-derives it. Credential-specific fields
// (CredentialKind, CredentialID) name the credential presented, not the
// subject.
type Principal struct {
	Subjects                     []string
	UserID, ServiceAccountID     string // exactly one set
	ProjectID, TeamID            string // empty for personal keys
	CredentialKind, CredentialID string // "key", key id
	KeyHash                      string
	Key                          *key.Key
	Policy                       *policy.Policy // nil → policy-less
	PassthroughAllowed           bool
	PayloadLogging               bool
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

// PrincipalMiddleware authenticates the inbound key according to the
// request's Mode classification (set by ClassifyMiddleware upstream) and
// resolves who it acts as:
//
//   - ModeNormal       — key is required; lookup must succeed.
//   - ModeProxyAuthed  — key is required; lookup must succeed.
//   - ModeProxyAnonymous — no key; this middleware is a no-op and
//     neither a *Key nor a *Principal is stashed on ctx.
//
// Snapshot is read on every request so admins toggling Enabled /
// RevokedAt take effect within the NOTIFY debounce window.
func PrincipalMiddleware(cat *appcatalog.Catalog) func(http.Handler) http.Handler {
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
			hash := hashToken(cls.Key)
			k, matchedPrevious := snap.KeyByHash(hash)
			if k == nil {
				writeAuthErr(w, "invalid api key")
				return
			}
			now := time.Now()
			switch {
			case !k.IsEnabled():
				writeAuthErr(w, "api key disabled")
				return
			case k.Spec.RevokedAt != nil:
				writeAuthErr(w, "api key revoked")
				return
			case k.Spec.ExpiresAt != nil && !now.Before(*k.Spec.ExpiresAt):
				writeAuthErr(w, "api key expired")
				return
			case matchedPrevious && !k.InGrace(now):
				writeAuthErr(w, "api key rotated")
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyT{}, k)
			ctx = context.WithValue(ctx, ctxPrincipalT{}, buildPrincipal(snap, k, hash))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// buildPrincipal resolves the key's identity and tenancy from the
// snapshot. Subjects stays nil: nothing evaluates it yet.
func buildPrincipal(snap *appcatalog.Snapshot, k *key.Key, hash string) *Principal {
	p := &Principal{
		CredentialKind:     "key",
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

func writeAuthErr(w http.ResponseWriter, msg string) {
	slog.Warn("inference: auth rejected", "status", 401, "code", "unauthenticated", "msg", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"unauthenticated","message":"` + msg + `"}}`))
}
