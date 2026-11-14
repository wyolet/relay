package inference

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/meta"
)

// mintFor runs the auth stack for plaintext and mints the lifecycle Context
// the dispatch entry would, so the assertions below see exactly what the
// usage observer reads.
func mintFor(t *testing.T, st *principalStack, plaintext string) (*Principal, *lifecycleFields) {
	t.Helper()
	if w := st.do(plaintext); w.Code != 200 {
		t.Fatalf("auth: status %d: %s", w.Code, w.Body.String())
	}
	p := st.seen
	if p == nil {
		t.Fatal("no principal resolved")
	}
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, p)
	lc := mintLifecycle(ctx, "pipeline", "")
	return p, &lifecycleFields{
		projectID: lc.ProjectID, project: lc.ProjectName,
		teamID: lc.TeamID, team: lc.TeamName,
		principalKind: lc.PrincipalKind, principalID: lc.PrincipalID, principal: lc.PrincipalName,
		credentialKind: lc.CredentialKind, credentialID: lc.CredentialID,
		relayKeyHash: lc.RelayKeyHash, relayKey: lc.RelayKeyName,
	}
}

type lifecycleFields struct {
	projectID, project           string
	teamID, team                 string
	principalKind, principalID   string
	principal                    string
	credentialKind, credentialID string
	relayKeyHash, relayKey       string
}

// What the usage observer reads off a request is the whole attribution and nothing borrowed: a field with no source stays empty.
func TestMintLifecycle_Attribution(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{boundTo(f, "token-binding", f.boundPol.Meta.ID, "user:"+f.user)}
	sa := saKey(f, "sk-sa")
	personal := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-laptop", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.user}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: f.user}, KeyHash: sha("sk-user")},
	}
	st := f.stack(t, sa, personal)

	for name, tc := range map[string]struct {
		bearer string
		want   lifecycleFields
	}{
		"service account key": {"sk-sa", lifecycleFields{
			projectID: f.project.Meta.ID, project: "ml-search", teamID: f.team.Meta.ID, team: "platform",
			principalKind: "serviceaccount", principalID: f.sa.Meta.ID, principal: "indexer",
			credentialKind: "key", credentialID: sa.Meta.ID, relayKeyHash: sha("sk-sa"), relayKey: "indexer-prod",
		}},
		// A personal key lives in no project, and users are not in the snapshot, so no tenancy and no principal slug.
		"personal key": {"sk-user", lifecycleFields{
			principalKind: "user", principalID: f.user,
			credentialKind: "key", credentialID: personal.Meta.ID, relayKeyHash: sha("sk-user"), relayKey: "alice-laptop",
		}},
		// A token presents no key: hashing the bearer would record a hash matching no key row and pull token traffic into a key's usage scope.
		"token": {f.mint(t, func(c *crypto.TokenClaims) { c.Jti = "jti-1" }), lifecycleFields{
			projectID: f.project.Meta.ID, project: "ml-search", teamID: f.team.Meta.ID, team: "platform",
			principalKind: "user", principalID: f.user,
			credentialKind: CredentialToken, credentialID: "jti-1",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, got := mintFor(t, st, tc.bearer); *got != tc.want {
				t.Errorf("attribution\n got %+v\nwant %+v", *got, tc.want)
			}
		})
	}
}

// Slugs are event-time facts: a rename after the request must not reach a
// Context already minted, and the next request picks the new name up.
func TestMintLifecycle_NamesAreEventTime(t *testing.T) {
	f := newPrincipalFixture()
	st := f.stack(t, saKey(f, "sk-sa"))
	_, before := mintFor(t, st, "sk-sa")

	f.project.Meta.Name = "ml-search-v2"
	renamedKey := saKey(f, "sk-sa")
	renamedKey.Meta.Name = "indexer-prod-v2"
	renamed := f.stack(t, renamedKey)
	_, after := mintFor(t, renamed, "sk-sa")

	if before.project != "ml-search" || before.relayKey != "indexer-prod" {
		t.Fatalf("minted-before names changed: %q / %q", before.project, before.relayKey)
	}
	if after.project != "ml-search-v2" || after.relayKey != "indexer-prod-v2" {
		t.Fatalf("minted-after names: want ml-search-v2 / indexer-prod-v2, got %q / %q", after.project, after.relayKey)
	}
	if before.projectID != after.projectID {
		t.Fatalf("project id must survive a rename: %q vs %q", before.projectID, after.projectID)
	}
}
