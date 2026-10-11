// Package authn turns a request into a principal: an ordered chain of authenticators, the product hooks that run once one succeeds, and the HTTP middleware that puts the result in the request context.
//
// It never mints account ids (ResolveUser maps an identity onto the product's own records) and never decides access: a request no authenticator claims passes through with no principal, and each route, or auth/rbac, decides what that caller may do.
package authn

import (
	"context"
	"errors"
	"net/http"

	"github.com/wyolet/relay/auth/principal"
)

// Authenticator recognises one credential format.
type Authenticator interface {
	// Authenticate returns (nil, false, nil) when the request carries no credential in its format, so the chain tries the next one. An error means the credential is in its format but is not accepted.
	Authenticate(r *http.Request) (*principal.Principal, bool, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(r *http.Request) (*principal.Principal, bool, error)

// Authenticate calls f.
func (f AuthenticatorFunc) Authenticate(r *http.Request) (*principal.Principal, bool, error) {
	return f(r)
}

// The rejections every product maps the same way; wrap one with fmt.Errorf("%w: ...") to add detail. Status maps each to its HTTP status.
var (
	// ErrInvalidCredential is a credential in a known format that is wrong, expired or revoked.
	ErrInvalidCredential = errors.New("authn: invalid credential")
	// ErrSessionDead is a session that no longer exists; the product clears its cookie.
	ErrSessionDead = errors.New("authn: session ended")
	// ErrInactive is a valid credential for an account that may not act: disabled, deleted or refused.
	ErrInactive = errors.New("authn: account inactive")
	// ErrUnavailable is a backend the check needs that did not answer; the credential may be fine.
	ErrUnavailable = errors.New("authn: backend unavailable")
)

// Chain runs authenticators in order. The first that claims the request decides it: its principal, or its error, is the chain's answer and no later authenticator runs.
type Chain struct {
	Authenticators []Authenticator
	// ResolveUser maps the authenticated identity onto the product's account, filling in what the credential did not carry (ID, Status, Roles). Optional.
	ResolveUser func(ctx context.Context, p *principal.Principal) error
	// Admit is the central status check, run after ResolveUser for every credential kind. Optional; AdmitActive is the usual one.
	Admit func(ctx context.Context, p *principal.Principal) error
}

// Authenticate returns the request's principal, nil when no authenticator claims it, or the error that rejected it.
func (c Chain) Authenticate(r *http.Request) (*principal.Principal, error) {
	for _, a := range c.Authenticators {
		p, claimed, err := a.Authenticate(r)
		if err != nil {
			return nil, err
		}
		if !claimed {
			continue
		}
		// A claim with no principal must not read as anonymous.
		if p == nil {
			return nil, ErrInvalidCredential
		}
		if c.ResolveUser != nil {
			if err := c.ResolveUser(r.Context(), p); err != nil {
				return nil, err
			}
		}
		if c.Admit != nil {
			if err := c.Admit(r.Context(), p); err != nil {
				return nil, err
			}
		}
		return p, nil
	}
	return nil, nil
}

// AdmitActive admits a principal whose Status is StatusActive and refuses every other, an unresolved one included.
func AdmitActive(_ context.Context, p *principal.Principal) error {
	if p.Status != principal.StatusActive {
		return ErrInactive
	}
	return nil
}

// Status is the HTTP status a rejection maps to. An error that is none of the package's is a server failure.
func Status(err error) int {
	switch {
	case errors.Is(err, ErrInvalidCredential), errors.Is(err, ErrSessionDead):
		return http.StatusUnauthorized
	case errors.Is(err, ErrInactive):
		return http.StatusForbidden
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Reject is the default rejection response: the status Status maps err to, with its status text and nothing about the error.
func Reject(w http.ResponseWriter, _ *http.Request, err error) {
	code := Status(err)
	http.Error(w, http.StatusText(code), code)
}

// Middleware runs c on every request. A principal goes into the request context (principal.From reads it); a rejection is answered by reject, Reject when nil; a request no authenticator claims passes through with no principal.
func Middleware(c Chain, reject func(w http.ResponseWriter, r *http.Request, err error)) func(http.Handler) http.Handler {
	if reject == nil {
		reject = Reject
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := c.Authenticate(r)
			if err != nil {
				reject(w, r, err)
				return
			}
			if p != nil {
				r = r.WithContext(principal.With(r.Context(), p))
			}
			next.ServeHTTP(w, r)
		})
	}
}
