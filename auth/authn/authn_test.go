package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/principal"
)

// fixed answers every request the same way and counts its calls.
type fixed struct {
	p       *principal.Principal
	claimed bool
	err     error
	calls   *int
}

func (f fixed) Authenticate(*http.Request) (*principal.Principal, bool, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.p, f.claimed, f.err
}

var notMine = fixed{}

func TestChainFirstClaimDecides(t *testing.T) {
	alice := &principal.Principal{ID: "alice"}
	bob := &principal.Principal{ID: "bob"}
	errBad := fmt.Errorf("%w: revoked", ErrInvalidCredential)
	var later int
	after := fixed{p: &principal.Principal{ID: "later"}, claimed: true, calls: &later}
	tests := []struct {
		name    string
		chain   []Authenticator
		want    *principal.Principal
		wantErr error
	}{
		{"none claims", []Authenticator{notMine, notMine}, nil, nil},
		{"no authenticators", nil, nil, nil},
		{"first claims", []Authenticator{fixed{p: alice, claimed: true}, after}, alice, nil},
		{"skips what is not its", []Authenticator{notMine, fixed{p: bob, claimed: true}, after}, bob, nil},
		{"a claiming error stops the chain", []Authenticator{fixed{claimed: true, err: errBad}, after}, nil, ErrInvalidCredential},
		{"an error without a claim stops it too", []Authenticator{fixed{err: ErrUnavailable}, after}, nil, ErrUnavailable},
		{"a claim with no principal fails closed", []Authenticator{fixed{claimed: true}, after}, nil, ErrInvalidCredential},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			later = 0
			c := Chain{Authenticators: tt.chain}
			got, err := c.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
			if !errors.Is(err, tt.wantErr) || (err == nil) != (tt.wantErr == nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("principal = %+v, want %+v", got, tt.want)
			}
			if later != 0 {
				t.Errorf("an authenticator after the deciding one ran %d times", later)
			}
		})
	}
}

func TestChainHooksRunInOrderAfterSuccess(t *testing.T) {
	var order []string
	c := Chain{
		Authenticators: []Authenticator{AuthenticatorFunc(func(*http.Request) (*principal.Principal, bool, error) {
			return &principal.Principal{Issuer: "https://idp", Subject: "s1"}, true, nil
		})},
		ResolveUser: func(_ context.Context, p *principal.Principal) error {
			order = append(order, "resolve")
			p.ID, p.Status = "u-1", principal.StatusActive
			return nil
		},
		Admit: func(ctx context.Context, p *principal.Principal) error {
			order = append(order, "admit")
			return AdmitActive(ctx, p)
		},
	}
	p, err := c.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil || p == nil || p.ID != "u-1" {
		t.Fatalf("Authenticate = %+v, %v; want the resolved principal", p, err)
	}
	if !slices.Equal(order, []string{"resolve", "admit"}) {
		t.Errorf("hooks ran %v, want resolve then admit", order)
	}

	// No claim, no hooks: an anonymous request has no identity to resolve.
	order = nil
	c.Authenticators = []Authenticator{notMine}
	if p, err := c.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); p != nil || err != nil || order != nil {
		t.Errorf("unclaimed request = %+v, %v, hooks %v; want nothing", p, err, order)
	}
}

func TestChainHookErrorsReject(t *testing.T) {
	claim := []Authenticator{fixed{p: &principal.Principal{ID: "u"}, claimed: true}}
	errLookup := errors.New("user store down")
	tests := []struct {
		name  string
		chain Chain
		want  error
	}{
		{"resolve fails", Chain{Authenticators: claim, ResolveUser: func(context.Context, *principal.Principal) error { return errLookup }}, errLookup},
		{"admission refuses", Chain{Authenticators: claim, Admit: AdmitActive}, ErrInactive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if p, err := tt.chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); p != nil || !errors.Is(err, tt.want) {
				t.Errorf("Authenticate = %+v, %v; want %v", p, err, tt.want)
			}
		})
	}
}

func TestAdmitActive(t *testing.T) {
	for _, s := range []principal.Status{"", principal.StatusDisabled, principal.StatusDeleted, "suspended"} {
		if err := AdmitActive(context.Background(), &principal.Principal{ID: "u", Status: s}); !errors.Is(err, ErrInactive) {
			t.Errorf("status %q admitted (err %v), want ErrInactive", s, err)
		}
	}
	for _, k := range []principal.CredentialKind{principal.CredentialSession, principal.CredentialAPIKey, principal.CredentialToken, principal.CredentialService} {
		p := &principal.Principal{ID: "u", Status: principal.StatusDisabled, Credential: principal.Credential{Kind: k}}
		if err := AdmitActive(context.Background(), p); !errors.Is(err, ErrInactive) {
			t.Errorf("a disabled account on a %s credential was admitted", k)
		}
	}
	if err := AdmitActive(context.Background(), &principal.Principal{ID: "u", Status: principal.StatusActive}); err != nil {
		t.Errorf("active account refused: %v", err)
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{ErrInvalidCredential, http.StatusUnauthorized},
		{fmt.Errorf("%w: expired", ErrInvalidCredential), http.StatusUnauthorized},
		{ErrSessionDead, http.StatusUnauthorized},
		{ErrInactive, http.StatusForbidden},
		{ErrUnavailable, http.StatusServiceUnavailable},
		{errors.New("other"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		if got := Status(tt.err); got != tt.want {
			t.Errorf("Status(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestMiddleware(t *testing.T) {
	alice := &principal.Principal{ID: "alice"}
	tests := []struct {
		name      string
		auth      Authenticator
		reject    func(http.ResponseWriter, *http.Request, error)
		status    int
		reached   bool
		principal *principal.Principal
	}{
		{"authenticated", fixed{p: alice, claimed: true}, nil, http.StatusOK, true, alice},
		{"unclaimed passes with no principal", notMine, nil, http.StatusOK, true, nil},
		{"rejected by the default", fixed{claimed: true, err: ErrUnavailable}, nil, http.StatusServiceUnavailable, false, nil},
		{"rejected by the product", fixed{claimed: true, err: ErrSessionDead}, func(w http.ResponseWriter, _ *http.Request, err error) {
			http.SetCookie(w, &http.Cookie{Name: "session", MaxAge: -1})
			w.WriteHeader(Status(err))
		}, http.StatusUnauthorized, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reached bool
			var got *principal.Principal
			h := Middleware(Chain{Authenticators: []Authenticator{tt.auth}}, tt.reject)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				reached, got = true, principal.From(r.Context())
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != tt.status || reached != tt.reached || got != tt.principal {
				t.Errorf("status %d reached %v principal %+v; want %d %v %+v", rec.Code, reached, got, tt.status, tt.reached, tt.principal)
			}
			if tt.reject != nil && len(rec.Result().Cookies()) != 1 {
				t.Errorf("product rejection did not clear the cookie")
			}
		})
	}
}
