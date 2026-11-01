package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

// The owner shapes a Policy accepts differ by rung: a host-owned row is a
// tier preset and links no pools or credentials, a project-, team- or
// user-owned row is a caller policy. Model refs are parsed at write time so
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
		{name: "project-owned is a caller policy", p: withOwner(meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()})},
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
			name: "host-owned tier linking a pool",
			p: func() *Policy {
				p := withOwner(meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()})
				p.Spec.PoolIDs = []string{meta.NewID()}
				return p
			}(),
			want: "must not list poolIds or credentialIds",
		},
		{
			name: "team owner",
			p:    withOwner(meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}),
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
			// Normalizing would drop the star and keep a prefix that grants nothing.
			name: "a star beside a name in the model position",
			p: func() *Policy {
				p := fix("p")
				p.Spec.Models = []string{"acme/model-*"}
				return p
			}(),
			want: "contains *",
		},
		{
			name: "a rate-limit scope the parser cannot read",
			p: func() *Policy {
				p := fix("p")
				p.Spec.RateLimits = []RateLimitLink{{Models: []string{"acme/model@"}, RateLimitID: meta.NewID()}}
				return p
			}(),
			want: "rateLimits[0]",
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
	p.Spec.RateLimits = []RateLimitLink{
		{Models: []string{"Acme/GPT-5.5@Host"}, RateLimitID: meta.NewID()},
	}
	if err := p.CanonicalizeSpecModelRefs(); err != nil {
		t.Fatalf("CanonicalizeSpecModelRefs: %v", err)
	}
	// The two spellings of one grant collapse to a single canonical entry.
	if want := []string{"acme/gpt-5-5", "acme"}; !slices.Equal(p.Spec.Models, want) {
		t.Errorf("models = %v, want %v", p.Spec.Models, want)
	}
	if got := p.Spec.RateLimits[0].Models; len(got) != 1 || got[0] != "acme/gpt-5-5@host" {
		t.Errorf("rateLimits models = %v, want [acme/gpt-5-5@host]", got)
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
	bad.Spec.RateLimits = []RateLimitLink{{Models: []string{"acme/model@"}, RateLimitID: meta.NewID()}}
	if err := bad.CanonicalizeSpecModelRefs(); err == nil || !strings.Contains(err.Error(), "rateLimits") {
		t.Errorf("err = %v, want one naming the rateLimits field", err)
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
