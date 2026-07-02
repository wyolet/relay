package control

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
)

func TestStampOwnerID(t *testing.T) {
	userCtx := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1", Username: "alice"})
	adminCtx := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})

	tests := []struct {
		name    string
		ctx     context.Context
		owner   meta.Owner
		wantID  string
		wantErr bool
	}{
		{
			name:   "user create stamps caller id",
			ctx:    userCtx,
			owner:  meta.Owner{Kind: meta.OwnerUser},
			wantID: "u-1",
		},
		{
			name:   "explicit truthful owner id allowed",
			ctx:    userCtx,
			owner:  meta.Owner{Kind: meta.OwnerUser, ID: "u-1"},
			wantID: "u-1",
		},
		{
			name:    "spoofed owner id rejected",
			ctx:     userCtx,
			owner:   meta.Owner{Kind: meta.OwnerUser, ID: "u-2"},
			wantErr: true,
		},
		{
			name:   "admin token keeps empty id (operator row)",
			ctx:    adminCtx,
			owner:  meta.Owner{Kind: meta.OwnerUser},
			wantID: "",
		},
		{
			name:   "admin token may set any owner id",
			ctx:    adminCtx,
			owner:  meta.Owner{Kind: meta.OwnerUser, ID: "u-2"},
			wantID: "u-2",
		},
		{
			name:   "non-user owner kinds untouched",
			ctx:    userCtx,
			owner:  meta.Owner{Kind: meta.OwnerHost, ID: "h-1"},
			wantID: "h-1",
		},
		{
			name:   "no actor in context is a no-op",
			ctx:    context.Background(),
			owner:  meta.Owner{Kind: meta.OwnerUser},
			wantID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := tt.owner
			err := stampOwnerID(tt.ctx, &o)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("stampOwnerID() = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("stampOwnerID() = %v, want nil", err)
			}
			if o.ID != tt.wantID {
				t.Fatalf("owner.ID = %q, want %q", o.ID, tt.wantID)
			}
		})
	}
}
