package policy

import (
	"errors"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/meta"
)

// problems reports why Validate refused: "Namespace tag" per failed field rule, or the message of a kind-specific rule.
func problems(err error) []string {
	var fields validator.ValidationErrors
	if errors.As(err, &fields) {
		var out []string
		for _, f := range fields {
			out = append(out, f.Namespace()+" "+f.Tag())
		}
		return out
	}
	if err != nil {
		return []string{err.Error()}
	}
	return nil
}

func fix(name string) *Policy {
	return &Policy{Meta: meta.Metadata{Name: name, Owner: meta.Owner{Kind: meta.OwnerUser}}}
}

func TestValidate(t *testing.T) {
	pool, limit := meta.NewID(), meta.NewID()
	models := func(refs ...string) func(*Policy) { return func(p *Policy) { p.Spec.Models = refs } }
	owner := func(kind meta.OwnerKind, id string) func(*Policy) {
		return func(p *Policy) { p.Meta.Owner = meta.Owner{Kind: kind, ID: id} }
	}
	const namesHost = " names a host; a policy names a provider or a provider's model, and its pools decide the hosts"
	for _, tc := range []struct {
		name string
		edit func(*Policy)
		want []string
	}{
		{"user-owned, empty spec", func(*Policy) {}, nil},
		{"system owner", owner(meta.OwnerSystem, ""), nil},
		{"team owner", owner(meta.OwnerTeam, "t-1"), nil},
		{"project owner", owner(meta.OwnerProject, "p-1"), nil},
		{"host owner: a tier preset", owner(meta.OwnerHost, "h-1"), nil},
		{"provider, model and provider wildcard grants", models("anthropic", "anthropic/claude-opus-4-7", "openai/*"), nil},
		{"a provider wildcard scopes a rate limit", func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: limit, Models: []string{"anthropic/*"}}, {RateLimitID: limit, Models: []string{"openai/*"}}}
		}, nil},

		{"missing name", func(p *Policy) { p.Meta.Name = "" }, []string{"Policy.Meta.Name required"}},
		{"pool id is not a UUID", func(p *Policy) { p.Spec.PoolIDs = []string{"pool-a"} }, []string{"Policy.Spec.PoolIDs[0] uuid"}},
		{"no owner", owner("", ""), []string{`policy "x": owner.kind required (user|system|host|team|project)`}},
		{"provider owner", owner(meta.OwnerProvider, "prov-1"), []string{`policy "x": owner.kind required (user|system|host|team|project)`}},
		{"host owner without its host", owner(meta.OwnerHost, ""), []string{`policy "x": owner.id is required for host-owned policy`}},
		{"host owner hands out no keys", func(p *Policy) {
			p.Meta.Owner, p.Spec.PoolIDs = meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}, []string{pool}
		},
			[]string{`policy "x": host-owned policies must not list poolIds`}},
		{"pool linked twice", func(p *Policy) { p.Spec.PoolIDs = []string{pool, pool} }, []string{`policy "x": duplicate poolIds entry "` + pool + `"`}},

		{"malformed model ref", models("anthropic", "a/b/c@"), []string{`policy "x": modelref "a/b/c@": trailing @ requires a host`}},
		{"model ref with a pattern", models("anthropic/claude-*"), []string{`policy "x": modelref "anthropic/claude-*": model "claude-*" contains *: refs take no patterns; leave the model out to match every one`}},
		{"model listed twice", models("anthropic", "openai", "anthropic"), []string{`policy "x": duplicate models entry "anthropic"`}},
		// A policy names providers and their models; hosts are the pools' business.
		{"binding ref", models("anthropic/claude-opus-4-7@bedrock"), []string{`policy "x": models entry "anthropic/claude-opus-4-7@bedrock"` + namesHost}},
		{"host ref", models("@bedrock"), []string{`policy "x": models entry "@bedrock"` + namesHost}},
		{"provider on a host", models("openai", "anthropic@bedrock"), []string{`policy "x": models entry "anthropic@bedrock"` + namesHost}},
		{"rate limit scoped to a host", func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: limit}, {RateLimitID: limit, Models: []string{"@bedrock"}}}
		}, []string{`rateLimits[1]: policy "x": models entry "@bedrock"` + namesHost}},
		{"one rate limit linked twice over the same models", func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: limit, Models: []string{"openai", "anthropic"}}, {RateLimitID: limit, Models: []string{"anthropic", "openai"}}}
		}, []string{`policy "x": rateLimits[0] and rateLimits[1] link rate limit "` + limit + `" over the same models`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fix("x")
			tc.edit(p)
			if diff := cmp.Diff(tc.want, problems(p.Validate())); diff != "" {
				t.Errorf("Validate (-want +got):\n%s", diff)
			}
		})
	}
}
