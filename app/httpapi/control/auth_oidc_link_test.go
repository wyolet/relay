package control

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
)

// newLinkOIDC is a harness with SCIM on, linking on the oid claim the fake provider signs as obj-1.
func newLinkOIDC(t *testing.T, registration string) *oidcHarness {
	t.Helper()
	idp := newFakeIdP(t)
	idp.claims = map[string]any{"oid": "obj-1"}
	h := newTestOIDC(t, idp, registration)
	h.scim = &settings.AuthSCIM{Enabled: true, LinkClaim: "oid"}
	return h
}

// wantLinked checks the account id signed in, holds the sign-in's identity, and the request recorded the login and the link.
func wantLinked(t *testing.T, h *oidcHarness, rec *httptest.ResponseRecorder, id string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status %d %s", rec.Code, rec.Body.String())
	}
	if h.sess.userID != id || h.users.links != 1 {
		t.Fatalf("session user %q, links %d; want %s linked once", h.sess.userID, h.users.links, id)
	}
	if u := h.users.bySubject[h.subject()]; u == nil || u.ID != id {
		t.Fatalf("identity %s belongs to %+v", h.subject(), u)
	}
	evs := h.events()
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want the login and the link", evs)
	}
	login, link := evs[0], evs[1]
	if login.Action != "auth.login" || login.Outcome.Status != audit.StatusAllowed || login.Resource.ID != id {
		t.Fatalf("login row = %+v", login)
	}
	if link.Action != "users.update" || link.Resource.ID != id || link.Outcome.Status != audit.StatusAllowed ||
		link.Actor.Kind != audit.ActorUser || link.Actor.ID != id || link.Change == nil || len(link.Change.Fields) != 1 || link.Change.Fields[0] != "identities" {
		t.Fatalf("link row = %+v", link)
	}
}

// A first sign-in whose link claim equals a SCIM-provisioned account's externalId signs in as that account and links the identity to it, with registration closed too.
func TestOIDCFlow_LinksProvisionedAccount(t *testing.T) {
	for _, registration := range []string{"closed", "open"} {
		t.Run(registration, func(t *testing.T) {
			h := newLinkOIDC(t, registration)
			h.users.provisioned(&user.User{ID: "u1", Username: "ada", Email: h.idp.email}, "obj-1")
			wantLinked(t, h, h.login("/auth/callback"), "u1")
		})
	}
	t.Run("default claim is sub", func(t *testing.T) {
		h := newTestOIDC(t, newFakeIdP(t), "closed")
		h.scim = &settings.AuthSCIM{Enabled: true}
		h.users.provisioned(&user.User{ID: "u1", Username: "ada"}, h.idp.sub)
		wantLinked(t, h, h.login("/api/auth/oidc/callback"), "u1")
	})
}

// The next sign-in finds the account by its identity and links nothing again.
func TestOIDCFlow_LinkedAccountSignsInAgain(t *testing.T) {
	h := newLinkOIDC(t, "closed")
	h.users.provisioned(&user.User{ID: "u1", Username: "ada"}, "obj-1")
	for range 2 {
		if rec := h.login("/auth/callback"); rec.Code != http.StatusFound {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if h.users.links != 1 || h.sess.calls != 2 || h.sess.userID != "u1" {
		t.Fatalf("links %d, session %+v", h.users.links, h.sess)
	}
	if evs := h.events(); len(evs) != 3 {
		t.Fatalf("events = %d, want two logins and one link", len(evs))
	}
}

// A matching email alone links nothing: the first sign-in is refused or provisioned as before.
func TestOIDCFlow_NoLinkOnEmailAlone(t *testing.T) {
	h := newLinkOIDC(t, "open")
	h.idp.claims = map[string]any{"oid": "obj-other"}
	h.users.provisioned(&user.User{ID: "u1", Username: "ada", Email: h.idp.email}, "obj-1")
	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusForbidden, "an account with this email already exists; an administrator must link it to this sign-in")
	if h.users.links != 0 || h.users.byName["ada"].OIDCSubject != "" || h.sess.calls != 0 {
		t.Fatalf("linked on email: links %d, account %+v", h.users.links, h.users.byName["ada"])
	}
	wantDenied(t, h.events(), h.subject())
}

// An account that already has a sign-in identity is not a link candidate.
func TestOIDCFlow_NoLinkWhenAccountHasIdentity(t *testing.T) {
	h := newLinkOIDC(t, "closed")
	h.users.provisioned(&user.User{ID: "u1", Username: "ada", OIDCSubject: "https://other.example|x"}, "obj-1")
	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusForbidden, "no account for this identity (registration is closed)")
	if h.users.links != 0 || h.users.byName["ada"].OIDCSubject != "https://other.example|x" {
		t.Fatalf("links %d, account %+v", h.users.links, h.users.byName["ada"])
	}
	wantDenied(t, h.events(), h.subject())
}

// A disabled match is refused like any disabled account and keeps no identity.
func TestOIDCFlow_DisabledProvisionedAccountRefused(t *testing.T) {
	h := newLinkOIDC(t, "open")
	h.users.provisioned(&user.User{ID: "u1", Username: "ada", Disabled: true}, "obj-1")
	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusUnauthorized, "account is disabled; contact your administrator")
	if h.users.links != 0 || h.users.byName["ada"].OIDCSubject != "" || h.sess.calls != 0 || len(h.users.byName) != 1 {
		t.Fatalf("links %d, accounts %+v, sessions %d", h.users.links, h.users.byName, h.sess.calls)
	}
	wantDenied(t, h.events(), "ada")
}

// Two provisioned accounts with the claimed id (two SCIM clients) refuse the sign-in and link neither.
func TestOIDCFlow_TwoProvisionedMatchesRefused(t *testing.T) {
	h := newLinkOIDC(t, "open")
	h.users.provisioned(&user.User{ID: "u1", Username: "ada"}, "obj-1")
	h.users.provisioned(&user.User{ID: "u2", Username: "ada-okta"}, "obj-1")
	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusConflict, "this sign-in matches more than one provisioned account; an administrator must resolve it")
	if h.users.links != 0 || h.sess.calls != 0 || len(h.users.byName) != 2 {
		t.Fatalf("links %d, accounts %d, sessions %d", h.users.links, len(h.users.byName), h.sess.calls)
	}
	wantDenied(t, h.events(), h.subject())
}

// With SCIM off nothing links: the taken email is the same 403 as before.
func TestOIDCFlow_NoLinkWithSCIMOff(t *testing.T) {
	h := newLinkOIDC(t, "open")
	h.scim.Enabled = false
	h.users.provisioned(&user.User{ID: "u1", Username: "ada", Email: h.idp.email}, "obj-1")
	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusForbidden, "an account with this email already exists; an administrator must link it to this sign-in")
	if h.users.links != 0 || h.users.byName["ada"].OIDCSubject != "" {
		t.Fatalf("linked with SCIM off: links %d", h.users.links)
	}
	wantDenied(t, h.events(), h.subject())
}

// The link claim is read from the verified id_token only: an inline profile links nothing even when its id matches.
func TestOIDCFlow_NoLinkFromInlineProfile(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "closed")
	h.idp.inlineProfile = true
	h.scim = &settings.AuthSCIM{Enabled: true}
	h.users.provisioned(&user.User{ID: "u1", Username: "ada"}, h.idp.sub)
	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusForbidden, "no account for this identity (registration is closed)")
	if h.users.links != 0 {
		t.Fatalf("linked from an inline profile")
	}
}

// Two first sign-ins race for the account: the one that links second finds the identity already on it, signs in as the same account and records no second link.
func TestOIDCFlow_ConcurrentFirstSignInLinksOnce(t *testing.T) {
	h := newLinkOIDC(t, "closed")
	h.users.provisioned(&user.User{ID: "u1", Username: "ada"}, "obj-1")
	h.users.beforeLink = func() {
		if rec := h.login("/auth/callback"); rec.Code != http.StatusFound {
			t.Errorf("racing sign-in: status %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := h.login("/auth/callback"); rec.Code != http.StatusFound {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if h.users.links != 1 || h.sess.calls != 2 || h.sess.userID != "u1" {
		t.Fatalf("links %d, session %+v", h.users.links, h.sess)
	}
	links := 0
	for _, ev := range h.events() {
		if ev.Action == "users.update" {
			links++
		}
	}
	if links != 1 {
		t.Fatalf("link rows = %d, want 1", links)
	}
}
