package refcheck

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

// modelScopeRows adds two system policies to the link rows: one whose rate limit counts per model, one whose limit covers the whole policy.
func modelScopeRows() (Checker, map[string]*policy.Policy) {
	rows := newLinkRows()
	system := meta.Owner{Kind: meta.OwnerSystem}
	policies := map[string]*policy.Policy{
		"per-model": {Meta: meta.Metadata{ID: "per-model", Name: "per-model", Owner: system},
			Spec: policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: "rl"}, {RateLimitID: "rl", Models: []string{"openai/gpt-4o"}}}}},
		"whole": {Meta: meta.Metadata{ID: "whole", Name: "whole", Owner: system},
			Spec: policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: "rl"}}}},
	}
	c := rows.checker(nil)
	c.Rows.Policy = func(_ context.Context, id string) *policy.Policy { return policies[id] }
	return c, policies
}

// A rate limit scoped to models is an upstream limit: binding its policy to a host, account or credential is fine, to a team, project, user or service account is refused, naming the policy and the limit.
func TestModelScopedLimitStaysOffCallerSubjects(t *testing.T) {
	c, _ := modelScopeRows()
	ctx := context.Background()
	bind := func(policyID string, sub rolebinding.Subject) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "b", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: policybinding.Spec{PolicyID: policyID, Subjects: []rolebinding.Subject{sub}}}
	}
	for _, tc := range []struct {
		policy string
		sub    rolebinding.Subject
		want   int
	}{
		{"per-model", rolebinding.Subject{Kind: policybinding.SubjectHost, ID: "h-1"}, 0},
		{"per-model", rolebinding.Subject{Kind: policybinding.SubjectAccount, ID: "acct-shared"}, 0},
		{"per-model", rolebinding.Subject{Kind: policybinding.SubjectCredential, ID: "key-h1"}, 0},
		{"per-model", rolebinding.Subject{Kind: policybinding.SubjectTeam, ID: "t-1"}, http.StatusBadRequest},
		{"per-model", rolebinding.Subject{Kind: policybinding.SubjectProject, ID: "p-1"}, http.StatusBadRequest},
		{"per-model", rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: meta.NewID()}, http.StatusBadRequest},
		{"per-model", rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: meta.NewID()}, http.StatusBadRequest},
		{"whole", rolebinding.Subject{Kind: policybinding.SubjectTeam, ID: "t-1"}, 0},
	} {
		err := c.PolicyBinding(ctx, bind(tc.policy, tc.sub))
		if got := status(err); got != tc.want {
			t.Errorf("%s on %s: status %d (%v), want %d", tc.policy, tc.sub.Key(), got, err, tc.want)
		}
		if tc.want != 0 && (!strings.Contains(err.Error(), `policy "per-model"`) || !strings.Contains(err.Error(), `rate limit "rl"`)) {
			t.Errorf("%s on %s: %v, want the policy and the rate limit named", tc.policy, tc.sub.Key(), err)
		}
	}
}

// A policy already bound to a caller subject cannot gain a models scope; bound only upstream, it can.
func TestModelScopeRefusedOnAPolicyBoundToACaller(t *testing.T) {
	c, policies := modelScopeRows()
	ctx := context.Background()
	var bound []*policybinding.PolicyBinding
	c.Rows.PolicyBindingsFor = func(_ context.Context, policyID string) ([]*policybinding.PolicyBinding, error) {
		var out []*policybinding.PolicyBinding
		for _, pb := range bound {
			if pb.Spec.PolicyID == policyID {
				out = append(out, pb)
			}
		}
		return out, nil
	}
	edited := *policies["whole"]
	edited.Spec.RateLimits = []policy.RateLimitLink{{RateLimitID: "rl", Models: []string{"openai/gpt-4o"}}}
	binding := func(sub rolebinding.Subject) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "team-binding", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: policybinding.Spec{PolicyID: "whole", Subjects: []rolebinding.Subject{sub}}}
	}

	bound = []*policybinding.PolicyBinding{binding(rolebinding.Subject{Kind: policybinding.SubjectAccount, ID: "acct-shared"})}
	if err := c.Policy(ctx, &edited); err != nil {
		t.Fatalf("models scope on a policy bound to an account: %v", err)
	}
	bound = append(bound, binding(rolebinding.Subject{Kind: policybinding.SubjectTeam, ID: "t-1"}))
	err := c.Policy(ctx, &edited)
	if status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), `policy binding "team-binding"`) {
		t.Fatalf("models scope on a policy bound to a team = %v, want 400 naming the binding", err)
	}
	if err := c.Policy(ctx, policies["whole"]); err != nil {
		t.Fatalf("a whole-policy limit on a team-bound policy: %v", err)
	}
}
