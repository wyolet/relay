package apply

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
)

// A group binds nobody to a policy, so apply refuses one as a subject, as the control API does.
func TestApplyRefusesAGroupPolicySubject(t *testing.T) {
	rows := tenancyRows(false)
	rows.Policies = []*policy.Policy{{Meta: meta.Metadata{ID: meta.NewID(), Name: "shared-policy", Owner: meta.Owner{Kind: meta.OwnerSystem}}}}
	b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
	doc := "kind: PolicyBinding\nmetadata: {name: ml-everyone, owner: {kind: project, name: ml-search}}\n" +
		"spec: {project: ml-search, policy: shared-policy, subjects: [{kind: group, name: \"system:authenticated\"}]}\n"
	wantRefused(t, b.run(context.Background(), userDocs(t, docHeader+doc)), "PolicyBinding", "ml-everyone", policybinding.ErrGroupSubject)
}
