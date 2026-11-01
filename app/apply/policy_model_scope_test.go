package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
)

// Apply runs the control API's rule from the policy side too: a document giving a team-bound policy a models scope is refused.
func TestApplyRefusesAModelScopeOnACallerBoundPolicy(t *testing.T) {
	rows := tenancyRows(false)
	system := meta.Owner{Kind: meta.OwnerSystem}
	rows.RateLimits = []*ratelimit.RateLimit{{Meta: meta.Metadata{ID: meta.NewID(), Name: "gpt-rpm", Owner: system}}}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "team-default", Owner: system}}
	rows.Policies = []*policy.Policy{pol}
	rows.PolicyBindings = []*policybinding.PolicyBinding{{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "platform-default", Owner: system},
		Spec: policybinding.Spec{PolicyID: pol.Meta.ID, Subjects: []rolebinding.Subject{{Kind: policybinding.SubjectTeam, ID: rows.Teams[0].Meta.ID}}},
	}}
	ctx := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
	plan := func(models string) error {
		b := &builder{opts: Options{Stores: &Stores{}, Authz: authz.AlwaysAllowAuthenticated{}}, rows: rows, idx: newIndex(rows), admin: true}
		doc := "kind: Policy\nmetadata: {name: team-default, owner: {kind: system}}\nspec: {rateLimits: [{ref: gpt-rpm" + models + "}]}\n"
		return b.run(ctx, userDocs(t, docHeader+doc))
	}
	if err := plan(""); err != nil {
		t.Fatalf("a whole-policy limit on a team-bound policy: %v", err)
	}
	if err := plan(", models: [openai/gpt-4o]"); err == nil || !strings.Contains(err.Error(), `policy binding "platform-default"`) {
		t.Fatalf("models scope on a team-bound policy = %v, want a refusal naming the binding", err)
	}
}
