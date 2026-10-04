package apply

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
)

// A user owner with no id names nobody. Applied by a caller that is no user
// (the admin token, the boot seed) the row is shared infrastructure and
// lands system-owned; applied by a user it becomes theirs, as on CRUD.
func TestApplyStampsAnIDlessUserOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want meta.Owner
	}{
		{"admin token", actor.WithActor(context.Background(), &actor.Actor{AdminToken: true}), meta.Owner{Kind: meta.OwnerSystem}},
		{"boot seed", context.Background(), meta.Owner{Kind: meta.OwnerSystem}},
		{"user session", actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"}), meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &builder{rows: &Rows{}, admin: true}
			b.idx = newIndex(b.rows)
			d := rateLimitDoc("ci-limit")
			d.Metadata.Owner.Kind = meta.OwnerUser
			var written []*ratelimit.RateLimit
			err := planKind(tc.ctx, b, kindWiring[manifest.RateLimitDTO, ratelimit.RateLimit]{
				Kind: "RateLimit", Docs: []*manifest.RateLimitDTO{d},
				Names: map[string]string{"ci-limit": meta.NewID()},
				To:    manifest.ToRateLimit,
				Meta:  func(x *ratelimit.RateLimit) *meta.Metadata { return &x.Meta },
				Upsert: func(_ context.Context, x *ratelimit.RateLimit) error {
					written = append(written, x)
					return nil
				},
				Delete: func(context.Context, string) error { return nil },
			})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			for _, e := range b.entries {
				if e.owner != tc.want {
					t.Fatalf("entry owner = %+v, want %+v", e.owner, tc.want)
				}
				if err := e.write(tc.ctx); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if len(written) != 1 || written[0].Meta.Owner != tc.want {
				t.Fatalf("written = %+v, want one row owned by %+v", written, tc.want)
			}
		})
	}
}
