package refcheck

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
)

// A refusal names no row the caller cannot see and no other user by name: they read as "an account you cannot see", "another team", "another user's".
func TestRefusalsHideWhatTheCallerCannotSee(t *testing.T) {
	_, base := wordRows()
	caller := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-caller", Username: "caller"})
	project := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	hiding := func(ids ...string) Checker {
		c := base
		h := hidingAuthz{hidden: map[string]bool{}}
		for _, id := range ids {
			h.hidden[id] = true
		}
		c.Authz = h
		return c
	}
	for _, tc := range []struct {
		name   string
		err    error
		says   string
		hidden []string
	}{
		{
			"a key naming an account the caller cannot see",
			hiding("acct-mine").KeyAccount(caller, &key.Key{Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: "u-caller"}, AccountID: "acct-mine"}}),
			"cannot run in an account you cannot see",
			[]string{"Anthropic org", "acct-mine", meID, "abror"},
		},
		{
			"a host key joining an account of a team the caller cannot see",
			hiding("t-1").HostKey(caller, &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: project}, Spec: hostkey.Spec{HostID: "h-1", AccountID: "acct-team-1"}}),
			`cannot join account "acct-team-1" of another team`,
			[]string{"Platform", `"t-1"`},
		},
		{
			"a host key joining another user's personal account",
			hiding().HostKey(caller, &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: project}, Spec: hostkey.Spec{HostID: "h-1", AccountID: "acct-mine"}}),
			`another user's personal account "Anthropic org"`,
			[]string{"abror", meID},
		},
		{
			"a binding of a policy whose pool the caller cannot see",
			hiding("pool-mine").PolicyBinding(actor.WithActor(context.Background(), &actor.Actor{UserID: meID}), &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "b", Owner: project}, Spec: policybinding.Spec{PolicyID: "pol-mine"}}),
			"uses a pool you cannot see",
			[]string{"anthropic", "pool-mine"},
		},
	} {
		got := refusalMsg(t, tc.err)
		if !strings.Contains(got, tc.says) {
			t.Errorf("%s: %s\nwant it to say %q", tc.name, got, tc.says)
		}
		for _, s := range tc.hidden {
			if strings.Contains(got, s) {
				t.Errorf("%s: names %q: %s", tc.name, s, got)
			}
		}
	}
}

// The picker runs the binding save's own checks: an account another binding holds is refused as the save refuses it, and the scope itself answers as a save's owner check does.
func TestUsableRunsTheWholeSaveCheck(t *testing.T) {
	_, c := wordRows()
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: meID})
	held := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: "pb-held", Name: "held"}, Spec: policybinding.Spec{AccountIDs: []string{"acct-p-1"}}}
	c.Rows.PolicyBindings = func(context.Context) ([]*policybinding.PolicyBinding, error) {
		return []*policybinding.PolicyBinding{held}, nil
	}
	p1 := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	a := c.Rows.Account(ctx, "acct-p-1")
	err := c.UsableAccount(ctx, a, p1)
	if status(err) != http.StatusConflict || code(err) != CodeDuplicateBinding {
		t.Errorf("an account another binding holds: %v, want 409 %s", err, CodeDuplicateBinding)
	}
	if save := c.PolicyBinding(ctx, &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "x", Owner: p1}, Spec: policybinding.Spec{AccountIDs: []string{"acct-p-1"}}}); status(save) != status(err) {
		t.Errorf("save answered %v, the picker %v", save, err)
	}

	hidden := c
	hidden.Authz = hidingAuthz{hidden: map[string]bool{"p-2": true}}
	c.Rows.Project = func(_ context.Context, id string) *project.Project {
		if id == "p-2" {
			return &project.Project{Meta: meta.Metadata{ID: "p-2", Name: "p-2"}, Spec: project.Spec{TeamID: "t-2"}}
		}
		return nil
	}
	hidden.Rows.Project = c.Rows.Project
	for _, tc := range []struct {
		name string
		c    Checker
		by   meta.Owner
		want int
	}{
		{"a project that does not exist", c, meta.Owner{Kind: meta.OwnerProject, ID: "p-9"}, http.StatusBadRequest},
		{"a project the caller cannot see", hidden, meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}, http.StatusNotFound},
		{"a team that does not exist", c, meta.Owner{Kind: meta.OwnerTeam, ID: "t-9"}, http.StatusBadRequest},
		{"another user", c, meta.Owner{Kind: meta.OwnerUser, ID: "u-else"}, http.StatusBadRequest},
		{"the caller", c, meta.Owner{Kind: meta.OwnerUser, ID: meID}, 0},
		{"the system", c, meta.Owner{Kind: meta.OwnerSystem}, 0},
	} {
		if got := status(tc.c.UsableScope(ctx, tc.by)); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}
