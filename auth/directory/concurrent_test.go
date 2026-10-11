package directory_test

import (
	"context"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
)

// interleaved runs between once, right after the next read of account id, so the reader goes on to write from a copy that is already stale.
type interleaved struct {
	directory.UserStore
	id      string
	between func()
}

func (s *interleaved) Get(ctx context.Context, id string) (*directory.User, error) {
	u, err := s.UserStore.Get(ctx, id)
	if id == s.id && s.between != nil {
		run := s.between
		s.between = nil
		run()
	}
	return u, err
}

func interleavedHarness(t *testing.T) (*harness, *interleaved) {
	t.Helper()
	s := memstore.New().Stores()
	users := &interleaved{UserStore: s.Users}
	s.Users = users
	h := &harness{s: s, grants: &grants{deny: map[string]bool{}}, notes: &notes{}, rev: &revoker{}}
	d, err := directory.New(directory.Config{Stores: s, Grants: h.grants, Notifier: h.notes, Revoker: h.rev})
	must(t, err)
	h.d = d
	return h, users
}

// An operator who disables a provisioned account, or takes a role from it, while the source's update runs keeps the change: the source writes only its own fields.
func TestProvisionerUpdateKeepsAConcurrentOperatorChange(t *testing.T) {
	h, users := interleavedHarness(t)
	addRole(t, h, "reader")
	p := provisioner(t, h, "okta")
	u, err := p.CreateUser(ctx, directory.ProvisionedUser{Username: "ada", ExternalID: "o-1"}, false)
	must(t, err)
	u.Roles = []string{"reader"}
	must(t, h.d.UpdateUser(ctx, admin, u))

	users.id, users.between = u.ID, func() {
		cur, _ := h.s.Users.Get(ctx, u.ID)
		cur.Roles = nil
		must(t, h.d.UpdateUser(ctx, admin, cur))
		must(t, h.d.DisableUser(ctx, u.ID))
	}
	if _, err := p.UpdateUser(ctx, u.ID, directory.ProvisionedUser{Username: "ada.lovelace", ExternalID: "o-1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := h.s.Users.Get(ctx, u.ID)
	if !got.Disabled || len(got.Roles) != 0 {
		t.Fatalf("the source's update undid the operator's change: %+v", got)
	}
	if got.Username != "ada.lovelace" {
		t.Fatalf("the source's own field was not written: %+v", got)
	}
}

// A directory update over an account that changed after it was read is a conflict, whether the caller carries the version it read or the directory's own read went stale.
func TestUpdateUserRefusesAStaleRead(t *testing.T) {
	h, users := interleavedHarness(t)
	addRole(t, h, "reader")
	u := &directory.User{Username: "ada"}
	must(t, h.d.CreateUser(ctx, nil, u))

	read, _ := h.s.Users.Get(ctx, u.ID)
	must(t, h.d.DisableUser(ctx, u.ID))
	read.Roles = []string{"reader"}
	wantErr(t, "the caller's read is stale", h.d.UpdateUser(ctx, admin, read), directory.ErrStaleResourceVersion)

	must(t, h.d.EnableUser(ctx, u.ID))
	next, _ := h.s.Users.Get(ctx, u.ID)
	users.id, users.between = u.ID, func() { must(t, h.d.DisableUser(ctx, u.ID)) }
	next.Roles, next.ResourceVersion = []string{"reader"}, ""
	wantErr(t, "the directory's read is stale", h.d.UpdateUser(ctx, admin, next), directory.ErrStaleResourceVersion)
	if got, _ := h.s.Users.Get(ctx, u.ID); !got.Disabled || len(got.Roles) != 0 {
		t.Fatalf("a refused update was written: %+v", got)
	}
}
