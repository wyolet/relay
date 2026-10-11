//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/auth/scim"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// Entra provisions an account through SCIM, then its user signs in through OIDC for the first time with the object id as oid: the sign-in lands on the provisioned account, its identity row is written, and the link is in the audit log.
func TestSCIMProvisionedAccountLinksOnFirstSignIn(t *testing.T) {
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
	}, directoryUsers{Store: stores.Users, ops: ops}, auditStore)
	h.scim = &settings.AuthSCIM{Enabled: true, LinkClaim: "oid"}

	if rec := h.login("/auth/callback"); rec.Code != http.StatusFound {
		t.Fatalf("first sign-in = %d: %s", rec.Code, rec.Body.String())
	}
	h.em.Close()
	if h.sess.userID != created.ID || h.sess.username != "ada@contoso.example" {
		t.Fatalf("signed in as %q (%s), want the provisioned account %s", h.sess.userID, h.sess.username, created.ID)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM identities WHERE issuer = $1 AND subject = $2`, idp.srv.URL, idp.sub).Scan(&owner); err != nil || owner != created.ID {
		t.Fatalf("identity row owner = %q, %v; want %s", owner, err, created.ID)
	}
	accounts, err := stores.Users.List(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %d, %v; want only the provisioned one", len(accounts), err)
	}

	evs, err := auditStore.Events(ctx, audit.Query{ResourceID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	var linked, loggedIn bool
	for _, ev := range evs {
		switch {
		case ev.Action == "users.update" && ev.Outcome.Status == audit.StatusAllowed && ev.Actor.ID == created.ID &&
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
