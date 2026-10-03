package inference

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
)

// legacyFixture moves the fixture's project onto the id the tenancy
// migration gives the project it parks pre-tenancy keys in, and strips the
// service account's own policy, which those keys never had.
func legacyFixture() principalFixture {
	f := newPrincipalFixture()
	f.project.Meta.ID = project.LegacyID
	f.project.Meta.Name = "legacy"
	f.sa.Spec.ProjectID = project.LegacyID
	f.sa.StampOwner()
	f.sa.Spec.PolicyID = ""
	for _, p := range []*policy.Policy{f.saPol, f.keyPol, f.boundPol} {
		p.Meta.Owner.ID = project.LegacyID
	}
	return f
}

func legacyKey(f principalFixture, plaintext string) *key.Key {
	k := saKey(f, plaintext)
	k.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: project.LegacyID}
	return k
}

// A key the migration moved onto the legacy project had no policy before
// the upgrade; it keeps reaching the policy-less flow, which routing gates
// on the inference setting exactly as it did.
func TestLegacyProjectKeyWithoutPolicyFallsThroughToPolicyless(t *testing.T) {
	f := legacyFixture()
	st := f.stack(t, legacyKey(f, "sk-legacy"))
	w := st.do("sk-legacy")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if st.seen == nil || st.seen.Policy != nil {
		t.Fatalf("principal = %+v, want one with no policy", st.seen)
	}
	if st.seen.ProjectID != project.LegacyID {
		t.Errorf("project = %q, want the legacy project for attribution", st.seen.ProjectID)
	}
}

// Once an operator binds a policy in the legacy project, its keys follow
// the same rule as every other project: resolve a policy or be refused.
func TestLegacyProjectKeyIsRefusedOnceTheProjectHasBindings(t *testing.T) {
	f := legacyFixture()
	f.bindings = []*policybinding.PolicyBinding{{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "someone-else", Owner: meta.Owner{Kind: meta.OwnerProject, ID: project.LegacyID}},
		Spec: policybinding.Spec{
			ProjectID: project.LegacyID, PolicyID: f.boundPol.Meta.ID,
			Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: meta.NewID()}},
		},
	}}
	w := f.stack(t, legacyKey(f, "sk-legacy")).do("sk-legacy")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body)
	}
}

// Any other project keeps the strict rule.
func TestProjectKeyWithoutPolicyIsRefused(t *testing.T) {
	f := newPrincipalFixture()
	f.sa.Spec.PolicyID = ""
	w := f.stack(t, saKey(f, "sk-plain")).do("sk-plain")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body)
	}
}
