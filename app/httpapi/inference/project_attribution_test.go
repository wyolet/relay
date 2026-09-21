package inference

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
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
