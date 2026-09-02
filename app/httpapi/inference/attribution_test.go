package inference

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
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
	lc := mintLifecycle(ctx, st.cat, "pipeline", plaintext, "")
	return p, &lifecycleFields{
		projectID: lc.ProjectID, project: lc.ProjectName,
		teamID: lc.TeamID, team: lc.TeamName,
		principalKind: lc.PrincipalKind, principalID: lc.PrincipalID, principal: lc.PrincipalName,
		credentialKind: lc.CredentialKind, credentialID: lc.CredentialID,
		relayKeyHash: lc.RelayKeyHash,
	}
}

type lifecycleFields struct {
	projectID, project           string
	teamID, team                 string
	principalKind, principalID   string
	principal                    string
	credentialKind, credentialID string
	relayKeyHash                 string
}

func TestMintLifecycle_ServiceAccountKeyCarriesFullAttribution(t *testing.T) {
	f := newPrincipalFixture()
	k := saKey(f, "sk-sa")
	st := f.stack(t, k)

	_, got := mintFor(t, st, "sk-sa")

	if got.projectID != f.project.Meta.ID || got.project != "ml-search" {
		t.Fatalf("project: %q / %q", got.projectID, got.project)
	}
	if got.teamID != f.team.Meta.ID || got.team != "platform" {
		t.Fatalf("team: %q / %q", got.teamID, got.team)
	}
	if got.principalKind != string(key.PrincipalServiceAccount) ||
		got.principalID != f.sa.Meta.ID || got.principal != "indexer" {
		t.Fatalf("principal: %q / %q / %q", got.principalKind, got.principalID, got.principal)
	}
	if got.credentialKind != "key" || got.credentialID != k.Meta.ID {
		t.Fatalf("credential: %q / %q", got.credentialKind, got.credentialID)
	}
	if got.relayKeyHash != sha("sk-sa") {
		t.Fatalf("relay_key_hash: %q", got.relayKeyHash)
	}
}

// A personal key names its user and the credential, but has no tenancy: it
// lives in no project, so project/team stay empty rather than borrowing one.
func TestMintLifecycle_PersonalKeyHasNoTenancy(t *testing.T) {
	f := newPrincipalFixture()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "abror-laptop",
			Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.user}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: f.user},
			KeyHash:   sha("sk-user"),
		},
	}
	st := f.stack(t, k)

	_, got := mintFor(t, st, "sk-user")

	if got.projectID != "" || got.project != "" || got.teamID != "" || got.team != "" {
		t.Fatalf("want empty tenancy, got %+v", got)
	}
	if got.principalKind != string(key.PrincipalUser) || got.principalID != f.user {
		t.Fatalf("principal: %q / %q", got.principalKind, got.principalID)
	}
	// Users are not in the snapshot, so no slug is resolvable at entry.
	if got.principal != "" {
		t.Fatalf("principal slug: want empty, got %q", got.principal)
	}
	if got.credentialKind != "key" || got.credentialID != k.Meta.ID {
		t.Fatalf("credential: %q / %q", got.credentialKind, got.credentialID)
	}
	if got.relayKeyHash != sha("sk-user") {
		t.Fatalf("relay_key_hash: %q", got.relayKeyHash)
	}
}

func TestMintLifecycle_TokenCredential(t *testing.T) {
	t.Skip("tokens land with M3: credential_kind=token, credential_id=jti, empty relay_key_hash")
}

// Slugs are event-time facts: a rename after the request must not reach a
// Context already minted, and the next request picks the new name up.
func TestMintLifecycle_NamesAreEventTime(t *testing.T) {
	f := newPrincipalFixture()
	st := f.stack(t, saKey(f, "sk-sa"))
	_, before := mintFor(t, st, "sk-sa")

	f.project.Meta.Name = "ml-search-v2"
	renamed := f.stack(t, saKey(f, "sk-sa"))
	_, after := mintFor(t, renamed, "sk-sa")

	if before.project != "ml-search" {
		t.Fatalf("minted-before name changed: %q", before.project)
	}
	if after.project != "ml-search-v2" {
		t.Fatalf("minted-after name: want ml-search-v2, got %q", after.project)
	}
	if before.projectID != after.projectID {
		t.Fatalf("project id must survive a rename: %q vs %q", before.projectID, after.projectID)
	}
}
