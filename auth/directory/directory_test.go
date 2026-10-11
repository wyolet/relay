package directory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

// *rbac.Engine is the grant checker products pass in.
var _ directory.GrantChecker = (*rbac.Engine)(nil)

type grantCall struct {
	roleID string
	scope  rbac.Scope
}

// grants records every check and refuses the role ids in deny to a caller that is not an admin.
type grants struct {
	deny  map[string]bool
	calls []grantCall
	err   error
}

func (g *grants) CheckGrant(_ context.Context, p *principal.Principal, role rbac.Role, scope rbac.Scope, _, _ []string) error {
	g.calls = append(g.calls, grantCall{role.ID, scope})
	if g.err != nil {
		return g.err
	}
	if !p.Authenticated() {
		return authz.ErrUnauthenticated
	}
	if g.deny[role.ID] && !p.Admin {
		return authz.ErrForbidden
	}
	return nil
}

type notes struct{ changes []directory.Change }

func (n *notes) Notify(_ context.Context, c directory.Change) { n.changes = append(n.changes, c) }

func (n *notes) last(t *testing.T) directory.Change {
	t.Helper()
	if len(n.changes) == 0 {
		t.Fatal("no change reported")
	}
	return n.changes[len(n.changes)-1]
}

type revoker struct {
	ended []string
	err   error
}

func (r *revoker) DestroyUser(_ context.Context, id string) error {
	r.ended = append(r.ended, id)
	return r.err
}

type harness struct {
	d      *directory.Directory
	s      directory.Stores
	grants *grants
	notes  *notes
	rev    *revoker
}

var (
	ctx    = context.Background()
	caller = &principal.Principal{ID: "caller", Kind: principal.KindUser}
	admin  = &principal.Principal{Admin: true, Kind: principal.KindAdmin}
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{s: memstore.New().Stores(), grants: &grants{deny: map[string]bool{}}, notes: &notes{}, rev: &revoker{}}
	d, err := directory.New(directory.Config{
		Stores: h.s, Grants: h.grants, Notifier: h.notes, Revoker: h.rev,
		Kinds: []string{"*", "documents", "folders"}, Verbs: []string{"*", "get", "list", "update"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.d = d
	return h
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

func TestNewRequiresStoresAndGrants(t *testing.T) {
	s := memstore.New().Stores()
	if _, err := directory.New(directory.Config{Stores: s}); err == nil {
		t.Fatal("no grant checker accepted")
	}
	s.Roles = nil
	if _, err := directory.New(directory.Config{Stores: s, Grants: &grants{}}); err == nil {
		t.Fatal("missing store accepted")
	}
	h := newHarness(t)
	if h.d.Stores().Users == nil {
		t.Fatal("Stores returned no users store")
	}
}

func TestMetadataVersion(t *testing.T) {
	if _, ok, err := (directory.Metadata{}).ExpectedVersion(); ok || err != nil {
		t.Fatalf("empty version = %v, %v", ok, err)
	}
	if v, ok, err := (directory.Metadata{ResourceVersion: "4"}).ExpectedVersion(); v != 4 || !ok || err != nil {
		t.Fatalf("version 4 = %d, %v, %v", v, ok, err)
	}
	if _, _, err := (directory.Metadata{ResourceVersion: "x"}).ExpectedVersion(); !errors.Is(err, directory.ErrStaleResourceVersion) {
		t.Fatalf("unparsable version: %v", err)
	}
	if id := directory.NewID(); len(id) != 36 || id[14] != '7' {
		t.Fatalf("NewID = %q, want a UUIDv7", id)
	}
}

func TestNames(t *testing.T) {
	cases := map[string]string{
		"OpenAI Prod": "openai-prod", "  hello   world  ": "hello-world", "Foo!!!Bar": "foo-bar",
		"---weird---": "weird", "": "", "已经-Mixed": "mixed",
	}
	for in, want := range cases {
		if got := directory.Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	long := directory.Slugify("a-" + string(repeat('b', 80)))
	if len(long) != directory.MaxNameLen || !directory.ValidName(long) {
		t.Fatalf("long slug %q (%d)", long, len(long))
	}
	taken := map[string]bool{"core": true, "core-2": true}
	if got := directory.UniqueName("core", func(n string) bool { return taken[n] }); got != "core-3" {
		t.Fatalf("UniqueName = %q", got)
	}
	full := string(repeat('a', directory.MaxNameLen))
	if got := directory.UniqueName(full, func(n string) bool { return n == full }); got != string(repeat('a', 61))+"-2" {
		t.Fatalf("UniqueName of a full-length name = %q", got)
	}
	for _, bad := range []string{"", "Upper", "-lead", "trail-", "a_b", string(repeat('a', 64))} {
		if directory.ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}

func repeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
