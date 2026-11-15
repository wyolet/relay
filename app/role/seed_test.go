package role

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/pkg/meta"
)

// The built-ins are the system's, validate against the vocabulary, and the scope admins who mint tokens can take them back.
func TestBuiltins(t *testing.T) {
	roles, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	var names []string
	byName := map[string]*Role{}
	for _, r := range roles {
		names = append(names, r.Meta.Name)
		byName[r.Meta.Name] = r
		if r.Meta.ID != "" || r.Meta.Owner != (meta.Owner{Kind: meta.OwnerSystem}) || r.Meta.DisplayName == "" || r.Meta.Description == "" {
			t.Errorf("built-in %q: metadata %+v, want no id, the system owner, a display name and a description", r.Meta.Name, r.Meta)
		}
		r.Meta.ID = "00000000-0000-7000-8000-000000000000"
		if err := r.Validate(); err != nil {
			t.Errorf("built-in %q does not validate: %v", r.Meta.Name, err)
		}
		if !IsBuiltin(r.Meta.Name) {
			t.Errorf("IsBuiltin(%q) = false", r.Meta.Name)
		}
	}
	if want := "admin auditor catalog-editor team-admin project-admin developer viewer"; strings.Join(names, " ") != want {
		t.Fatalf("built-ins = %v, want %s", names, want)
	}
	if IsBuiltin("custom") || IsBuiltin("") {
		t.Error("IsBuiltin accepts a name that is no built-in")
	}
	for _, tc := range []struct {
		role, kind, verb string
		want             bool
	}{
		{"admin", "teams", "delete", true},
		{BuiltinTeamAdmin, "tokens", "mint", true},
		{BuiltinTeamAdmin, "tokens", "revoke", true},
		{BuiltinTeamAdmin, "projects", "delete", true},
		{BuiltinTeamAdmin, "teams", "delete", false},
		{BuiltinProjectAdmin, "tokens", "mint", true},
		{BuiltinProjectAdmin, "tokens", "revoke", true},
		{BuiltinProjectAdmin, "projects", "create", false},
		{"auditor", "audit", "read", true},
		{"auditor", "keys", "get", false},
		{"catalog-editor", "models", "update", true},
		{"catalog-editor", "host-keys", "get", false},
	} {
		if got := byName[tc.role].Allows(tc.kind, tc.verb); got != tc.want {
			t.Errorf("%s allows %s.%s = %v, want %v", tc.role, tc.kind, tc.verb, got, tc.want)
		}
	}
}

// refusingDB records whether the store was reached and fails every query, so the seed stops at its first read.
type refusingDB struct{ queried bool }

func (d *refusingDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("refusingDB")
}

func (d *refusingDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	d.queried = true
	return nil, errors.New("refusingDB")
}

func (d *refusingDB) QueryRow(context.Context, string, ...interface{}) pgx.Row { return nil }

func (d *refusingDB) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("refusingDB")
}

func (d *refusingDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("refusingDB")
}

// Pods booting together must not each mint an id for the same missing role, so every read and write of the seed runs inside the caller's lock; the lock's own failure is the seed's.
func TestSeedBuiltinsRunsInsideTheLock(t *testing.T) {
	db := &refusingDB{}
	var trace []string
	lock := func(ctx context.Context, fn func(context.Context) error) error {
		if db.queried {
			t.Error("the store was read before the lock was taken")
		}
		trace = append(trace, "locked")
		err := fn(ctx)
		if !db.queried {
			t.Error("the seed never reached the store")
		}
		trace = append(trace, "unlocked")
		return err
	}
	err := SeedBuiltins(context.Background(), NewStore(db), nil, lock)
	if err == nil || !strings.HasPrefix(err.Error(), "role seed: list: ") || !strings.HasSuffix(err.Error(), "refusingDB") {
		t.Errorf("SeedBuiltins = %v, want the store's failure as role seed: list: … refusingDB", err)
	}
	if strings.Join(trace, " ") != "locked unlocked" {
		t.Errorf("lock trace = %v, want locked then unlocked", trace)
	}

	busy := errors.New("lock busy")
	unlocked := &refusingDB{}
	err = SeedBuiltins(context.Background(), NewStore(unlocked), nil, func(context.Context, func(context.Context) error) error { return busy })
	if !errors.Is(err, busy) || unlocked.queried {
		t.Errorf("a lock that fails: err %v, store reached %v; want the lock's error and no read", err, unlocked.queried)
	}
	if err := SeedBuiltins(context.Background(), NewStore(unlocked), nil, nil); err == nil || !unlocked.queried {
		t.Errorf("no lock: err %v, store reached %v; want the seed to run unserialized", err, unlocked.queried)
	}
	if err := SeedBuiltins(context.Background(), nil, nil, lock); err != nil || len(trace) != 2 {
		t.Errorf("no store: err %v, lock taken %d times; want a no-op", err, len(trace)/2)
	}
}

// A rule set that differs from the embedded file must be detected, or a rule added in a release never reaches an upgraded deployment.
func TestSameRules(t *testing.T) {
	rule := func(kinds string, verbs ...string) Rule { return Rule{Kinds: strings.Fields(kinds), Verbs: verbs} }
	a := []Rule{rule("tokens keys", "mint", "get")}
	for _, tc := range []struct {
		name string
		b    []Rule
		want bool
	}{
		{"identical", []Rule{rule("tokens keys", "mint", "get")}, true},
		{"an added verb", []Rule{rule("tokens keys", "mint", "get", "revoke")}, false},
		{"a dropped kind", []Rule{rule("tokens", "mint", "get")}, false},
		{"reordered verbs", []Rule{rule("tokens keys", "get", "mint")}, false},
		{"reordered kinds", []Rule{rule("keys tokens", "mint", "get")}, false},
		{"an added rule", []Rule{rule("tokens keys", "mint", "get"), rule("teams", "get")}, false},
		{"no rules", nil, false},
	} {
		if got := sameRules(a, tc.b); got != tc.want {
			t.Errorf("%s: sameRules = %v, want %v", tc.name, got, tc.want)
		}
		if got := sameRules(tc.b, a); got != tc.want {
			t.Errorf("%s, swapped: sameRules = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !sameRules(nil, []Rule{}) {
		t.Error("two empty rule sets differ")
	}
}
