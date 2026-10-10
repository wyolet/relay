package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
)

func hostDoc(name string, owner manifest.WireOwner) *manifest.HostDTO {
	d := &manifest.HostDTO{APIVersion: manifest.APIVersion, Kind: "Host"}
	d.Metadata.Name = name
	d.Metadata.Owner = owner
	d.Spec.BaseURL = "https://upstream.example"
	return d
}

func planHost(ctx context.Context, admin bool, d *manifest.HostDTO) ([]Entry, error) {
	b := &builder{rows: &Rows{}, admin: admin}
	_, err := planOne(ctx, b, kindWiring[manifest.HostDTO, host.Host]{
		Kind: "Host", Docs: []*manifest.HostDTO{d},
		Names: map[string]string{d.Metadata.Name: meta.NewID()},
		To:    manifest.ToHost,
		Meta:  func(x *host.Host) *meta.Metadata { return &x.Meta },
	})
	return b.entries, err
}

// The catalog's own documents mark an operator-configured host with an
// id-less user owner. Whoever applies one, the host lands system-owned:
// a host is never anyone's personal row.
func TestApplyHostWithIDlessUserOwnerIsSystemOwned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		admin bool
	}{
		{"boot seed", context.Background(), true},
		{"admin token", actor.WithActor(context.Background(), &actor.Actor{AdminToken: true}), true},
		{"admin session", actor.WithActor(context.Background(), &actor.Actor{UserID: "u-root", Roles: []string{user.RoleAdmin}}), true},
		{"user session", actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := planHost(tc.ctx, tc.admin, hostDoc("ollama-self", manifest.WireOwner{Kind: meta.OwnerUser}))
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if len(entries) != 1 || entries[0].owner != (meta.Owner{Kind: meta.OwnerSystem}) {
				t.Fatalf("entries = %+v, want one system-owned host", entries)
			}
		})
	}
}

// A document naming a real tenant owner for a host is refused, for the
// admin as for anyone: the rule is about the row, not the caller.
func TestApplyRefusesTenantOwnedHost(t *testing.T) {
	session := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	for _, owner := range []manifest.WireOwner{
		{Kind: meta.OwnerUser, ID: "u-1"},
		{Kind: meta.OwnerTeam, ID: meta.NewID()},
		{Kind: meta.OwnerProject, ID: meta.NewID()},
	} {
		for _, admin := range []bool{false, true} {
			_, err := planHost(session, admin, hostDoc("box", owner))
			var inv *InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("owner %+v admin=%v: err = %v, want InvalidError", owner, admin, err)
			}
		}
	}
}

// A HostBinding document follows the same rule.
func TestApplyRefusesTenantOwnedHostBinding(t *testing.T) {
	session := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	for _, owner := range []manifest.WireOwner{
		{Kind: meta.OwnerUser, ID: "u-1"},
		{Kind: meta.OwnerProject, ID: meta.NewID()},
	} {
		b := &builder{rows: &Rows{}}
		d := &manifest.HostBindingDTO{APIVersion: manifest.APIVersion, Kind: "HostBinding"}
		d.Metadata.Name = "m-on-box"
		d.Metadata.Owner = owner
		_, err := planOne(session, b, kindWiring[manifest.HostBindingDTO, binding.Binding]{
			Kind: "HostBinding", Docs: []*manifest.HostBindingDTO{d},
			Names: map[string]string{d.Metadata.Name: meta.NewID()},
			// The owner rule is what is under test, not name resolution.
			To: func(d manifest.HostBindingDTO, _ manifest.Resolver) (*binding.Binding, error) {
				return &binding.Binding{
					Meta: meta.Metadata{Name: d.Metadata.Name, Owner: meta.Owner{Kind: d.Metadata.Owner.Kind, ID: d.Metadata.Owner.ID}},
					Spec: binding.Spec{ModelID: meta.NewID(), HostID: meta.NewID()},
				}, nil
			},
			Meta: func(x *binding.Binding) *meta.Metadata { return &x.Meta },
		})
		var inv *InvalidError
		if !errors.As(err, &inv) {
			t.Fatalf("owner %+v: err = %v, want InvalidError", owner, err)
		}
	}
}
