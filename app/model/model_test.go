package model

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func validProvID() string { return meta.NewID() }

// fix builds a minimally-valid Model. Tests override fields they want to break.
func fix(name string) *Model {
	return &Model{
		Meta: meta.Metadata{
			Name:  name,
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: validProvID()},
		},
		Spec: Spec{
			Snapshots: []Snapshot{{Name: name + "-snap", OriginalName: name + "-snap"}},
			Pointer:   name + "-snap",
		},
	}
}

func TestValidate(t *testing.T) {
	t.Run("ok minimal", func(t *testing.T) {
		if err := fix("gpt-4o").Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		m    *Model
		want string
	}{
		{
			name: "missing name",
			m:    func() *Model { m := fix("x"); m.Meta.Name = ""; return m }(),
			want: "Name",
		},
		{
			name: "user owner rejected",
			m:    func() *Model { m := fix("x"); m.Meta.Owner.Kind = meta.OwnerUser; return m }(),
			want: "owner.kind must be provider",
		},
		{
			name: "system owner rejected",
			m:    func() *Model { m := fix("x"); m.Meta.Owner.Kind = meta.OwnerSystem; return m }(),
			want: "owner.kind must be provider",
		},
		{
			name: "owner id required",
			m:    func() *Model { m := fix("x"); m.Meta.Owner.ID = ""; return m }(),
			want: "owner.id is required",
		},
		{
			name: "missing snapshots",
			m:    func() *Model { m := fix("x"); m.Spec.Snapshots = nil; m.Spec.Pointer = ""; return m }(),
			want: "Snapshots",
		},
		{
			name: "missing pointer",
			m:    func() *Model { m := fix("x"); m.Spec.Pointer = ""; return m }(),
			want: "Pointer",
		},
		{
			name: "duplicate snapshot",
			m: func() *Model {
				m := fix("x")
				dup := m.Spec.Snapshots[0]
				m.Spec.Snapshots = append(m.Spec.Snapshots, dup)
				return m
			}(),
			want: "duplicate snapshot",
		},
		{
			name: "pointer does not match any snapshot",
			m: func() *Model {
				m := fix("x")
				m.Spec.Pointer = "nope"
				return m
			}(),
			want: "pointer \"nope\" does not match",
		},
		{
			name: "bad deprecation status",
			m: func() *Model {
				m := fix("x")
				m.Spec.Deprecation = &Deprecation{Status: "bogus"}
				return m
			}(),
			want: "Status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestSnapshotUpstream(t *testing.T) {
	cases := []struct {
		name string
		s    Snapshot
		want string
	}{
		{"empty originalName falls back to name", Snapshot{Name: "gpt-4o-2024-11-20"}, "gpt-4o-2024-11-20"},
		{"originalName carries the upstream form", Snapshot{Name: "gpt-5-5", OriginalName: "gpt-5.5"}, "gpt-5.5"},
		{"colons and slashes preserved in original", Snapshot{Name: "ollama-llama2-7b", OriginalName: "ollama/llama2:7b"}, "ollama/llama2:7b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Upstream(); got != tc.want {
				t.Errorf("Upstream() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateAliases(t *testing.T) {
	withAliases := func(aliases ...string) *Model {
		m := fix("claude-fable-5")
		m.Spec.Aliases = aliases
		return m
	}
	t.Run("valid exact and pattern", func(t *testing.T) {
		m := withAliases("claude-fable-5[1m]", "claude-fable-5[*]", "ft:gpt*:acme")
		if err := m.Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	for _, tc := range []struct {
		name    string
		aliases []string
		want    string
	}{
		{"empty alias", []string{""}, "empty alias"},
		{"bare star", []string{"*"}, "literal prefix"},
		{"leading star", []string{"*-mini"}, "literal prefix"},
		{"two stars", []string{"a*b*c"}, "at most one"},
		{"pattern prefix no usable chars", []string{"[[*x"}, "no usable characters"},
		{"exact no usable chars", []string{"[]"}, "no usable characters"},
		{"dup normalized exact", []string{"x[1m]", "x.1m"}, "duplicate alias"},
		{"dup normalized pattern", []string{"x[*]", "x.*"}, "duplicate alias"},
		{"shadows own snapshot", []string{"claude.fable.5.snap"}, "always win"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := withAliases(tc.aliases...).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	t.Run("truncation collision caught", func(t *testing.T) {
		base := strings.Repeat("a", 62)
		// Both normalize to the same 63-char-truncated slug.
		m := withAliases(base+"[1m]", base+"[2m]")
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate alias") {
			t.Fatalf("want truncation dup error, got %v", err)
		}
	})
}

func TestAliasPattern(t *testing.T) {
	for _, tc := range []struct {
		alias     string
		pre, suf  string
		isPattern bool
	}{
		{"claude-fable-5[*]", "claude-fable-5-", "", true},
		{"ft:gpt*:acme", "ft-gpt", "-acme", true},
		{"claude-fable-5[1m]", "", "", false},
	} {
		pre, suf, ok := AliasPattern(tc.alias)
		if ok != tc.isPattern || pre != tc.pre || suf != tc.suf {
			t.Errorf("AliasPattern(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.alias, pre, suf, ok, tc.pre, tc.suf, tc.isPattern)
		}
	}
}

func TestIsEnabled(t *testing.T) {
	tru, fls := true, false
	for _, tc := range []struct {
		name string
		val  *bool
		want bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", &tru, true},
		{"explicit false", &fls, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{Spec: Spec{Enabled: tc.val}}
			if got := m.IsEnabled(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
