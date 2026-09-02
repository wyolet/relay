package inference

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/crypto"
)

// A personal key pointed at a project-owned policy spends that project's
// upstream credentials, so the request must carry the project's attribution
// and limits rather than looking like an unscoped personal call.
func TestPersonalKeyOnAProjectPolicyCarriesTheProject(t *testing.T) {
	f := newPrincipalFixture()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "personal", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.user}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: f.user},
			PolicyID:  f.keyPol.Meta.ID,
			KeyHash:   sha("sk-wr-personal"),
		},
	}
	st := f.stack(t, k)
	if w := st.do("sk-wr-personal"); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if st.seen == nil {
		t.Fatal("no principal resolved")
	}
	if st.seen.ProjectID != f.project.Meta.ID {
		t.Errorf("projectID = %q, want the policy's project %q", st.seen.ProjectID, f.project.Meta.ID)
	}
	if st.seen.TeamID != f.team.Meta.ID {
		t.Errorf("teamID = %q, want %q", st.seen.TeamID, f.team.Meta.ID)
	}
}

// Signing out everywhere (a token-version bump) answers the same code as a
// per-jti revocation, so a client has one thing to key on.
func TestTokenVersionRevocationAnswersTokenRevoked(t *testing.T) {
	f := newPrincipalFixture()
	token := f.mint(t, func(c *crypto.TokenClaims) { c.Ver = 0 }) // stale version
	w := f.stack(t).do(token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "token_revoked" {
		t.Errorf("code = %q, want token_revoked", body.Error.Code)
	}
}

// Recheck runs per WebSocket frame: a credential scoped to a project that
// has left the snapshot must stop working, not keep spending its limits.
func TestRecheckRefusesAMissingProject(t *testing.T) {
	f := newPrincipalFixture()
	st := f.stack(t, saKey(f, "sk-wr-live"))
	snap := st.cat.Current()

	p := &Principal{CredentialKind: CredentialKey, KeyHash: sha("sk-wr-live"), ProjectID: f.project.Meta.ID}
	if err := p.Recheck(snap, time.Now()); err != nil {
		t.Fatalf("Recheck with a live project: %v", err)
	}
	if err := st.cat.ApplyProjectDelete(f.project.Meta.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if err := p.Recheck(st.cat.Current(), time.Now()); err == nil {
		t.Fatal("Recheck accepted a credential whose project is gone")
	}
}

// The policy a frame routes on is resolved against that frame's snapshot: a
// binding written after the upgrade reaches a live connection.
func TestFramePolicyFollowsTheFrameSnapshot(t *testing.T) {
	f := newPrincipalFixture()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rebound", Owner: meta.Owner{Kind: meta.OwnerProject, ID: f.project.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: f.sa.Meta.ID},
			PolicyID:  f.keyPol.Meta.ID,
			KeyHash:   sha("sk-wr-rebound"),
		},
	}
	st := f.stack(t, k)
	if w := st.do("sk-wr-rebound"); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	upgrade := st.seen
	if upgrade.Policy == nil || upgrade.Policy.Meta.ID != f.keyPol.Meta.ID {
		t.Fatalf("upgrade policy = %v, want key-pol", upgrade.Policy)
	}

	// Rebind the key to another policy; the frame path re-reads the key
	// from the frame's snapshot, so the next frame routes on the new one.
	rebound := *k
	rebound.Spec.PolicyID = f.boundPol.Meta.ID
	if err := st.cat.ApplyKeyUpsert(&rebound); err != nil {
		t.Fatalf("rebind key: %v", err)
	}
	frame := framePrincipal(upgrade, st.cat.Current())
	if frame.Policy == nil || frame.Policy.Meta.ID != f.boundPol.Meta.ID {
		t.Fatalf("frame policy = %v, want bound-pol", frame.Policy)
	}
	// The connection's own principal is untouched — frames run in parallel.
	if upgrade.Policy.Meta.ID != f.keyPol.Meta.ID {
		t.Error("the shared principal was mutated by a frame")
	}
}
