package directory_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/password"
)

func TestAddIdentity(t *testing.T) {
	h := newHarness(t)
	u := &directory.User{Username: "ada"}
	must(t, h.d.CreateUser(ctx, nil, u))
	idp := directory.UserIdentity{Identity: directory.Identity{Issuer: "https://idp.example.com", Subject: "s-1"}, ExternalID: "obj-1"}

	must(t, h.d.AddIdentity(ctx, u.ID, idp))
	if c := h.notes.last(t); c != (directory.Change{Kind: directory.KindUser, Op: directory.OpUpsert, ID: u.ID}) {
		t.Fatalf("change = %+v", c)
	}
	got, _ := h.s.Users.Get(ctx, u.ID)
	if got.Identity != idp.Identity || len(got.Identities) != 1 || got.Identities[0].ExternalID != "obj-1" {
		t.Fatalf("after the first identity: %+v", got)
	}
	found, _ := h.s.Users.ByIdentity(ctx, idp.Identity)
	if found == nil || found.ID != u.ID {
		t.Fatalf("sign-in lookup = %+v", found)
	}

	other := &directory.User{Username: "bob"}
	must(t, h.d.CreateUser(ctx, nil, other))
	wantErr(t, "identity of another user", h.d.AddIdentity(ctx, other.ID, idp), directory.ErrConflict)
	wantErr(t, "missing user", h.d.AddIdentity(ctx, directory.NewID(), idp), directory.ErrNotFound)
	wantErr(t, "zero identity", h.d.AddIdentity(ctx, u.ID, directory.UserIdentity{}), directory.ErrInvalid)
	wantErr(t, "half an identity", h.d.AddIdentity(ctx, u.ID, directory.UserIdentity{Identity: directory.Identity{Subject: "s"}}), directory.ErrInvalid)
	wantErr(t, "separator in issuer", h.d.AddIdentity(ctx, u.ID,
		directory.UserIdentity{Identity: directory.Identity{Issuer: "a|b", Subject: "s"}}), directory.ErrInvalid)
	wantErr(t, "primary of another user", h.d.UpdateUser(ctx, nil, &directory.User{ID: other.ID, Username: "bob", Identity: idp.Identity}), directory.ErrConflict)
}

// Only accounts a source created and no identity reached yet are link candidates, matched on the source's id alone.
func TestUnlinkedUsersByExternalID(t *testing.T) {
	h := newHarness(t)
	entra, _ := h.d.Provisioner("entra")
	okta, _ := h.d.Provisioner("okta")
	ada, err := entra.CreateUser(ctx, directory.ProvisionedUser{Username: "ada", Email: "ada@example.com", ExternalID: "obj-1"}, false)
	must(t, err)
	linked, err := entra.CreateUser(ctx, directory.ProvisionedUser{Username: "bob", ExternalID: "obj-2"}, false)
	must(t, err)
	must(t, h.d.AddIdentity(ctx, linked.ID, directory.UserIdentity{Identity: directory.Identity{Issuer: "https://idp.example.com", Subject: "bob"}}))
	must(t, h.d.CreateUser(ctx, nil, &directory.User{Username: "local", Email: "obj-1"}))

	ids := func(externalID string) []string {
		t.Helper()
		got, err := h.d.UnlinkedUsersByExternalID(ctx, externalID)
		must(t, err)
		var out []string
		for _, u := range got {
			out = append(out, u.Username)
		}
		return out
	}
	if got := ids("obj-1"); !slices.Equal(got, []string{ada.Username}) {
		t.Fatalf("obj-1 = %v, want ada", got)
	}
	if got := ids("obj-2"); len(got) != 0 {
		t.Fatalf("an account with an identity is a candidate: %v", got)
	}
	if got := ids(""); len(got) != 0 {
		t.Fatalf("empty id = %v", got)
	}
	_, err = okta.CreateUser(ctx, directory.ProvisionedUser{Username: "ada-okta", ExternalID: "obj-1"}, false)
	must(t, err)
	if got := ids("obj-1"); !slices.Equal(got, []string{"ada", "ada-okta"}) {
		t.Fatalf("two sources = %v", got)
	}
}

func TestAddFirstIdentity(t *testing.T) {
	h := newHarness(t)
	entra, _ := h.d.Provisioner("entra")
	u, err := entra.CreateUser(ctx, directory.ProvisionedUser{Username: "ada", ExternalID: "obj-1"}, false)
	must(t, err)
	id := directory.Identity{Issuer: "https://idp.example.com", Subject: "s-1"}

	wantErr(t, "half an identity", h.d.AddFirstIdentity(ctx, u.ID, directory.Identity{Subject: "s"}), directory.ErrInvalid)
	wantErr(t, "zero identity", h.d.AddFirstIdentity(ctx, u.ID, directory.Identity{}), directory.ErrInvalid)
	wantErr(t, "missing user", h.d.AddFirstIdentity(ctx, directory.NewID(), id), directory.ErrNotFound)
	must(t, h.d.AddFirstIdentity(ctx, u.ID, id))
	if c := h.notes.last(t); c != (directory.Change{Kind: directory.KindUser, Op: directory.OpUpsert, ID: u.ID}) {
		t.Fatalf("change = %+v", c)
	}
	got, _ := h.s.Users.ByIdentity(ctx, id)
	if got == nil || got.ID != u.ID || got.Identity != id {
		t.Fatalf("after the link: %+v", got)
	}
	wantErr(t, "account already linked", h.d.AddFirstIdentity(ctx, u.ID, directory.Identity{Issuer: id.Issuer, Subject: "s-2"}), directory.ErrConflict)
}

// Two first sign-ins racing to link one account: exactly one identity is linked.
func TestAddFirstIdentityConcurrent(t *testing.T) {
	h := newHarness(t)
	entra, _ := h.d.Provisioner("entra")
	u, err := entra.CreateUser(ctx, directory.ProvisionedUser{Username: "ada", ExternalID: "obj-1"}, false)
	must(t, err)
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			errs <- h.d.AddFirstIdentity(ctx, u.ID, directory.Identity{Issuer: "https://idp.example.com", Subject: fmt.Sprintf("s-%d", i)})
		})
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, directory.ErrConflict):
			t.Fatalf("err = %v", err)
		}
	}
	got, _ := h.s.Users.Get(ctx, u.ID)
	if ok != 1 || len(got.Identities) != 1 {
		t.Fatalf("%d links succeeded, identities %+v", ok, got.Identities)
	}
}

func TestRemoveIdentity(t *testing.T) {
	h := newHarness(t)
	first := directory.Identity{Issuer: "https://idp.example.com", Subject: "s-1"}
	second := directory.UserIdentity{Identity: directory.Identity{Issuer: "https://other.example.com", Subject: "o-1"}}
	u := &directory.User{Username: "ada", Identity: first}
	must(t, h.d.CreateUser(ctx, nil, u))
	must(t, h.d.AddIdentity(ctx, u.ID, second))

	must(t, h.d.RemoveIdentity(ctx, u.ID, first))
	if !slices.Equal(h.rev.ended, []string{u.ID}) {
		t.Fatalf("sessions ended for %v, want the user's", h.rev.ended)
	}
	got, _ := h.s.Users.Get(ctx, u.ID)
	if got.Identity != second.Identity || len(got.Identities) != 1 {
		t.Fatalf("after removing the primary: %+v", got)
	}
	wantErr(t, "identity not held", h.d.RemoveIdentity(ctx, u.ID, first), directory.ErrNotFound)
	wantErr(t, "missing user", h.d.RemoveIdentity(ctx, directory.NewID(), first), directory.ErrNotFound)
	wantErr(t, "last identity without a password", h.d.RemoveIdentity(ctx, u.ID, second.Identity), directory.ErrInvalid)

	hash, err := password.Hash("s3cret")
	must(t, err)
	got.PasswordHash = hash
	must(t, h.d.UpdateUser(ctx, nil, got))
	must(t, h.d.RemoveIdentity(ctx, u.ID, second.Identity))
	got, _ = h.s.Users.Get(ctx, u.ID)
	if !got.Identity.IsZero() || got.Identities != nil {
		t.Fatalf("after removing the last identity: %+v", got)
	}
}

func TestRemoveIdentities(t *testing.T) {
	h := newHarness(t)
	u := &directory.User{Username: "ada", Identity: directory.Identity{Issuer: "https://idp.example.com", Subject: "s-1"}}
	must(t, h.d.CreateUser(ctx, nil, u))
	must(t, h.d.AddIdentity(ctx, u.ID, directory.UserIdentity{Identity: directory.Identity{Issuer: "https://idp.example.com", Subject: "s-2"}}))
	wantErr(t, "every identity of an account without a password", h.d.RemoveIdentities(ctx, u.ID), directory.ErrInvalid)
	wantErr(t, "missing user", h.d.RemoveIdentities(ctx, directory.NewID()), directory.ErrNotFound)

	hash, err := password.Hash("s3cret")
	must(t, err)
	got, _ := h.s.Users.Get(ctx, u.ID)
	got.PasswordHash = hash
	must(t, h.d.UpdateUser(ctx, nil, got))
	must(t, h.d.RemoveIdentities(ctx, u.ID))
	if !slices.Equal(h.rev.ended, []string{u.ID}) {
		t.Fatalf("sessions ended for %v, want the user's", h.rev.ended)
	}
	if got, _ := h.s.Users.Get(ctx, u.ID); !got.Identity.IsZero() || got.Identities != nil {
		t.Fatalf("after removing every identity: %+v", got)
	}
	must(t, h.d.RemoveIdentities(ctx, u.ID))
}

func TestUpdateUserKeepsExternal(t *testing.T) {
	h := newHarness(t)
	ext := directory.External{Source: "scim-1", ID: "e-1"}
	u := &directory.User{Username: "ada", External: ext}
	must(t, h.d.CreateUser(ctx, nil, u))
	must(t, h.d.UpdateUser(ctx, nil, &directory.User{ID: u.ID, Username: "ada", Email: "ada@example.com"}))
	got, _ := h.s.Users.Get(ctx, u.ID)
	if got.External != ext || got.Email != "ada@example.com" {
		t.Fatalf("after an update that leaves external out: %+v", got)
	}
	moved := directory.External{Source: "scim-1", ID: "e-2"}
	must(t, h.d.UpdateUser(ctx, nil, &directory.User{ID: u.ID, Username: "ada", External: moved}))
	if got, _ := h.s.Users.Get(ctx, u.ID); got.External != moved {
		t.Fatalf("external = %+v, want %+v", got.External, moved)
	}
	wantErr(t, "id without a source", h.d.CreateUser(ctx, nil, &directory.User{Username: "bob", External: directory.External{ID: "x"}}), directory.ErrInvalid)
}
