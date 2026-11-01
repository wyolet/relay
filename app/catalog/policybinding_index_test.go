package catalog

import (
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

// PolicyBinding subject keys are precomputed at build and at every
// incremental write, so the request path never renders them.
func TestPolicyBindingSubjectKeysArePrecomputed(t *testing.T) {
	c, _, proj, pol := tenancySnap(t)
	b := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "b1"}}
	b.Spec.ProjectID = proj.Meta.ID
	b.Spec.PolicyID = pol.Meta.ID
	b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "data-science"}}
	b.StampOwner()

	if err := c.ApplyPolicyBindingUpsert(b); err != nil {
		t.Fatalf("apply binding: %v", err)
	}
	got := c.Current().PolicyBindingsForProject(proj.Meta.ID)
	if len(got) != 1 {
		t.Fatalf("bindings = %d, want 1", len(got))
	}
	if len(got[0].SubjectKeys) != 1 || got[0].SubjectKeys[0] != b.Spec.Subjects[0].Key() {
		t.Fatalf("SubjectKeys = %v, want %q", got[0].SubjectKeys, b.Spec.Subjects[0].Key())
	}
}
