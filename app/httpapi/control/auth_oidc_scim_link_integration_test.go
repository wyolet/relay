//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/auth/scim"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// scimLink is ada, provisioned through an Entra SCIM client, and an OIDC harness whose first sign-in carries her object id as oid.
type scimLink struct {
	h       *oidcHarness
	stores  *appcatalog.Stores
	pool    *pgxpool.Pool
	idp     *fakeIdP
	audit   *audit.Store
	created string
}

func newSCIMLink(t *testing.T) *scimLink {
	t.Helper()
	ctx := t.Context()
	pool := storagetest.Pool(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	ops := newDirectoryOps(Deps{Stores: stores})
	dir, err := ops.directory()
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := scim.CreateClient(ctx, stores.SCIMClients, "entra")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := scim.New(scim.Config{Directory: dir, Tokens: stores.SCIMClients})
	if err != nil {
		t.Fatal(err)
	}

	const objectID = "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef"
	req := httptest.NewRequest(http.MethodPost, "/Users", strings.NewReader(`{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
		"externalId": "`+objectID+`",
		"userName": "ada@contoso.example",
		"active": true,
		"emails": [{"primary": true, "type": "work", "value": "ada@contoso.example"}]
	}`))
	req.Header.Set("Content-Type", "application/scim+json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	sp.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("SCIM create = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("SCIM create: %v %s", err, rec.Body.String())
	}

	idp := newFakeIdP(t)
	idp.email = "ada@contoso.example"
	idp.claims = map[string]any{"oid": objectID}
	auditStore := audit.NewStore(gen.New(pool))
	h := newOIDCHarnessOver(t, idp, &settings.AuthOIDC{
		Enabled: true, Issuer: idp.srv.URL, ClientID: "client-1",
		RedirectURL: "http://relay.test/auth/callback", Registration: "closed",
	}, directoryUsers{Store: stores.Users, ops: ops, clients: stores.SCIMClients}, auditStore)
	h.scim = &settings.AuthSCIM{Enabled: true, LinkClaim: "oid"}
	return &scimLink{h: h, stores: stores, pool: pool, idp: idp, audit: auditStore, created: created.ID}
}

// identityOwner returns the account holding the fake IdP's sign-in identity and how many rows hold it.
func (l *scimLink) identityOwner(t *testing.T) (owner string, n int) {
	t.Helper()
	rows, err := l.pool.Query(t.Context(), `SELECT user_id FROM identities WHERE issuer = $1 AND subject = $2`, l.idp.srv.URL, l.idp.sub)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&owner); err != nil {
			t.Fatal(err)
		}
		n++
	}
	return owner, n
}

// Entra provisions an account through SCIM, then its user signs in through OIDC for the first time with the object id as oid: the sign-in lands on the provisioned account, its identity row is written, and the link is in the audit log.
func TestSCIMProvisionedAccountLinksOnFirstSignIn(t *testing.T) {
	ctx := t.Context()
	l := newSCIMLink(t)
	if rec := l.h.login("/auth/callback"); rec.Code != http.StatusFound {
		t.Fatalf("first sign-in = %d: %s", rec.Code, rec.Body.String())
	}
	l.h.em.Close()
	if l.h.sess.userID != l.created || l.h.sess.username != "ada@contoso.example" {
		t.Fatalf("signed in as %q (%s), want the provisioned account %s", l.h.sess.userID, l.h.sess.username, l.created)
	}
	if owner, n := l.identityOwner(t); n != 1 || owner != l.created {
		t.Fatalf("identity rows = %d owned by %q; want one owned by %s", n, owner, l.created)
	}
	accounts, err := l.stores.Users.List(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %d, %v; want only the provisioned one", len(accounts), err)
	}

	evs, err := l.audit.Events(ctx, audit.Query{ResourceID: l.created})
	if err != nil {
		t.Fatal(err)
	}
	var linked, loggedIn bool
	for _, ev := range evs {
		switch {
		case ev.Action == "users.update" && ev.Outcome.Status == audit.StatusAllowed && ev.Actor.ID == l.created &&
			ev.Change != nil && len(ev.Change.Fields) == 1 && ev.Change.Fields[0] == "identities":
			linked = true
		case ev.Action == "auth.login" && ev.Outcome.Status == audit.StatusAllowed:
			loggedIn = true
		}
	}
	if !linked || !loggedIn {
		t.Fatalf("audit rows %+v lack the link or the login", evs)
	}
}

// An account a revoked SCIM client provisioned is no longer the identity provider's to hand out: a first sign-in that matches it links nothing, and with registration closed it is refused.
func TestSCIMRevokedClientsAccountsDoNotLink(t *testing.T) {
	l := newSCIMLink(t)
	if err := l.stores.SCIMClients.RevokeClient(t.Context(), "entra"); err != nil {
		t.Fatal(err)
	}
	if rec := l.h.login("/auth/callback"); rec.Code != http.StatusForbidden {
		t.Fatalf("first sign-in after the client was revoked = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	l.h.em.Close()
	if _, n := l.identityOwner(t); n != 0 {
		t.Fatalf("the revoked client's account was linked (%d identity rows)", n)
	}
}
