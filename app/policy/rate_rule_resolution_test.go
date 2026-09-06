package policy

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
)

func testRateLimit(id string, rules ...appratelimit.Rule) *appratelimit.RateLimit {
	rl := &appratelimit.RateLimit{}
	rl.Meta.ID, rl.Meta.Name = id, id
	rl.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem}
	rl.Spec.Rules = rules
	return rl
}

func requestsRule() appratelimit.Rule {
	return appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 10,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow,
	}
}

// RLBindings are scanned in declared order and the first match wins, so an
// author who overlaps two binding model sets gets the earlier one — never
// both, and never the later one. The flat RateLimitID is the fallback for a
// request no binding covers, and it never buckets per model.
func TestSelectRateLimitID_FirstMatchWins(t *testing.T) {
	overlapping := fix("p")
	overlapping.Spec.RLBindings = []RLBinding{
		{Models: []string{"acme/fast"}, RateLimitID: "rl-narrow"},
		{Models: []string{"acme"}, RateLimitID: "rl-broad"},
	}

	for _, tc := range []struct {
		name                              string
		pol                               *Policy
		providerSlug, modelSlug, hostSlug string
		wantID                            string
		wantPerModel                      bool
	}{
		{
			name: "the earlier binding wins over a later one that also matches",
			pol:  overlapping, providerSlug: "acme", modelSlug: "fast", hostSlug: "h",
			wantID: "rl-narrow", wantPerModel: true,
		},
		{
			name: "a model only the broad binding covers takes it",
			pol:  overlapping, providerSlug: "acme", modelSlug: "slow", hostSlug: "h",
			wantID: "rl-broad", wantPerModel: true,
		},
		{
			name: "a model no binding covers is uncapped at this policy",
			pol:  overlapping, providerSlug: "other", modelSlug: "fast", hostSlug: "h",
			wantID: "", wantPerModel: false,
		},
		{
			name: "the flat rate limit applies to every model and buckets once",
			pol: func() *Policy {
				p := fix("p")
				p.Spec.RateLimitID = "rl-flat"
				return p
			}(),
			providerSlug: "acme", modelSlug: "anything", hostSlug: "h",
			wantID: "rl-flat", wantPerModel: false,
		},
		{
			name: "a policy with neither is uncapped",
			pol:  fix("p"), providerSlug: "acme", modelSlug: "fast", hostSlug: "h",
			wantID: "", wantPerModel: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, perModel := tc.pol.SelectRateLimitID(tc.providerSlug, tc.modelSlug, tc.hostSlug)
			if id != tc.wantID || perModel != tc.wantPerModel {
				t.Fatalf("SelectRateLimitID = (%q, %v), want (%q, %v)", id, perModel, tc.wantID, tc.wantPerModel)
			}
		})
	}

	// A nil policy is the policy-less caller; it caps nothing rather than
	// panicking on the RLBindings walk.
	if id, perModel := (*Policy)(nil).SelectRateLimitID("acme", "fast", "h"); id != "" || perModel {
		t.Fatalf("nil policy = (%q, %v), want (\"\", false)", id, perModel)
	}
}

// D72: the bucket a rule counts in is named by the policy, the RateLimit it
// came from and — for a per-model binding — the model, so no two grants of
// one policy can collide in the same counter. The key's exact rendering is
// pinned by TestRuleKeyFormat_CarriesRateLimitAndModel; what this adds is
// that no two of the five combinations ever share one.
func TestResolveRules_KeysDistinctPerRateLimitAndModel(t *testing.T) {
	pol := fix("prod")
	rlA := testRateLimit("rl-a", requestsRule())
	rlB := testRateLimit("rl-b", requestsRule())

	seen := map[string]string{}
	for _, tc := range []struct {
		label   string
		rl      *appratelimit.RateLimit
		modelID string
		want    string
	}{
		{"flat, rl-b", rlB, "", "policy:prod:rl:rl-b:0:requests"},
		{"rl-a on model one", rlA, "m-1", "policy:prod:rl:rl-a:m:m-1:0:requests"},
		{"rl-a on model two", rlA, "m-2", "policy:prod:rl:rl-a:m:m-2:0:requests"},
		{"rl-b on model one", rlB, "m-1", "policy:prod:rl:rl-b:m:m-1:0:requests"},
	} {
		rules := pol.ResolveRules(tc.rl, tc.modelID)
		if len(rules) != 1 {
			t.Fatalf("%s: %d rules, want 1", tc.label, len(rules))
		}
		if rules[0].Key != tc.want {
			t.Errorf("%s: key = %q, want %q", tc.label, rules[0].Key, tc.want)
		}
		if other, dup := seen[rules[0].Key]; dup {
			t.Errorf("%s shares bucket %q with %s", tc.label, rules[0].Key, other)
		}
		seen[rules[0].Key] = tc.label
	}

	// Every rule of one RateLimit gets its own index, so two rules of the
	// same meter family stay separate counters.
	multi := pol.ResolveRules(testRateLimit("rl-c", requestsRule(), requestsRule()), "")
	if len(multi) != 2 || multi[0].Key == multi[1].Key {
		t.Fatalf("keys = %v, want two distinct buckets", keysOf(multi))
	}

	// Nothing to resolve: no rate limit, a disabled one, or an empty rule
	// set all mean this policy caps nothing here.
	if got := pol.ResolveRules(nil, ""); got != nil {
		t.Errorf("nil rate limit = %v, want nil", keysOf(got))
	}
	off := false
	disabled := testRateLimit("rl-off", requestsRule())
	disabled.Spec.Enabled = &off
	if got := pol.ResolveRules(disabled, ""); got != nil {
		t.Errorf("disabled rate limit = %v, want nil", keysOf(got))
	}
	if got := pol.ResolveRules(testRateLimit("rl-empty"), ""); got != nil {
		t.Errorf("rate limit with no rules = %v, want nil", keysOf(got))
	}
	if got := (*Policy)(nil).ResolveRules(rlA, ""); got != nil {
		t.Errorf("nil policy = %v, want nil", keysOf(got))
	}
}

// The owner shapes a Policy accepts differ by tier: a host-owned row is an
// upstream tier definition and carries no inbound grants, a project- or
// user-owned row is a customer policy. Model refs are parsed at write time so
// a grant that can never match is refused rather than silently ignored.
func TestValidate_OwnerShapesAndModelRefs(t *testing.T) {
	withOwner := func(o meta.Owner) *Policy {
		p := fix("p")
		p.Meta.Owner = o
		return p
	}

	for _, tc := range []struct {
		name string
		p    *Policy
		want string // empty means the row is accepted
	}{
		{name: "project-owned is a customer policy", p: withOwner(meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()})},
		{
			name: "host-owned tier with an id and no grants",
			p:    withOwner(meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()}),
		},
		{
			name: "host-owned tier without an id",
			p:    withOwner(meta.Owner{Kind: meta.OwnerHost}),
			want: "owner.id is required for host-owned policy",
		},
		{
			name: "host-owned tier listing host keys",
			p: func() *Policy {
				p := withOwner(meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()})
				p.Spec.HostKeyIDs = []string{meta.NewID()}
				return p
			}(),
			want: "must not list hostKeyIds",
		},
		{
			name: "team owner is not a policy owner",
			p:    withOwner(meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}),
			want: "owner.kind required",
		},
		{
			name: "a model ref the parser cannot read",
			p: func() *Policy {
				p := fix("p")
				p.Spec.Models = []string{"acme/model@"}
				return p
			}(),
			want: `policy "p"`,
		},
		{
			name: "a duplicate model ref",
			p: func() *Policy {
				p := fix("p")
				p.Spec.Models = []string{"acme/m", "acme/m"}
				return p
			}(),
			want: "duplicate models entry",
		},
		{
			name: "provider-wide, host-only and host-pinned refs are accepted",
			p: func() *Policy {
				p := fix("p")
				p.Spec.Models = []string{"acme", "@host", "acme/m@host", "acme/m"}
				return p
			}(),
		},
		{
			// A literal "*" is not the wildcard spelling — an absent
			// segment is — so it is refused rather than granting everything.
			name: "a literal star in the model position",
			p: func() *Policy {
				p := fix("p")
				p.Spec.Models = []string{"acme/*"}
				return p
			}(),
			want: "slug-compatible",
		},
		{
			name: "an rlBinding model ref the parser cannot read is not checked here",
			p: func() *Policy {
				p := fix("p")
				p.Spec.RLBindings = []RLBinding{{Models: []string{"acme/model@"}, RateLimitID: meta.NewID()}}
				return p
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want the row accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Every writer of a Policy row stores the same string for the same grant, or
// apply reports an update forever against a row the API just normalised.
func TestCanonicalizeSpecModelRefs_RewritesEveryGrantList(t *testing.T) {
	p := fix("p")
	p.Spec.Models = []string{"Acme/GPT-5.5", "acme/gpt-5-5", "Acme"}
	p.Spec.RLBindings = []RLBinding{
		{Models: []string{"Acme/GPT-5.5@Host"}, RateLimitID: meta.NewID()},
	}
	if err := p.CanonicalizeSpecModelRefs(); err != nil {
		t.Fatalf("CanonicalizeSpecModelRefs: %v", err)
	}
	// The two spellings of one grant collapse to a single canonical entry.
	if want := []string{"acme/gpt-5-5", "acme"}; !slices.Equal(p.Spec.Models, want) {
		t.Errorf("models = %v, want %v", p.Spec.Models, want)
	}
	if got := p.Spec.RLBindings[0].Models; len(got) != 1 || got[0] != "acme/gpt-5-5@host" {
		t.Errorf("rlBinding models = %v, want [acme/gpt-5-5@host]", got)
	}
	// Canonicalizing twice is a no-op: the second pass is what apply's diff
	// compares against.
	before := append([]string(nil), p.Spec.Models...)
	if err := p.CanonicalizeSpecModelRefs(); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !slices.Equal(p.Spec.Models, before) {
		t.Errorf("second pass changed models: %v → %v", before, p.Spec.Models)
	}

	bad := fix("bad")
	bad.Spec.Models = []string{"acme/model@"}
	if err := bad.CanonicalizeSpecModelRefs(); err == nil || !strings.Contains(err.Error(), "models") {
		t.Errorf("err = %v, want one naming the models field", err)
	}
	bad = fix("bad")
	bad.Spec.RLBindings = []RLBinding{{Models: []string{"acme/model@"}, RateLimitID: meta.NewID()}}
	if err := bad.CanonicalizeSpecModelRefs(); err == nil || !strings.Contains(err.Error(), "rlBindings") {
		t.Errorf("err = %v, want one naming the rlBindings field", err)
	}
}

func TestIsEnabled(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		set  *bool
		want bool
	}{
		{"unset defaults to on", nil, true},
		{"explicit on", &on, true},
		{"explicit off", &off, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fix("p")
			p.Spec.Enabled = tc.set
			if got := p.IsEnabled(); got != tc.want {
				t.Errorf("IsEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}
