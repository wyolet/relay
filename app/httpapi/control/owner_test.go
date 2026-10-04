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

	system := meta.Owner{Kind: meta.OwnerSystem}
	tests := []struct {
		name    string
		ctx     context.Context
		owner   meta.Owner
		want    meta.Owner
		wantErr bool
	}{
		{
			name:  "user create stamps caller id",
			ctx:   userCtx,
			owner: meta.Owner{Kind: meta.OwnerUser},
			want:  meta.Owner{Kind: meta.OwnerUser, ID: "u-1"},
		},
		{
			name:  "explicit truthful owner id allowed",
			ctx:   userCtx,
			owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-1"},
			want:  meta.Owner{Kind: meta.OwnerUser, ID: "u-1"},
		},
		{
			name:    "spoofed owner id rejected",
			ctx:     userCtx,
			owner:   meta.Owner{Kind: meta.OwnerUser, ID: "u-2"},
			wantErr: true,
		},
		{
			name:  "admin token without an owner id creates a system row",
			ctx:   adminCtx,
			owner: meta.Owner{Kind: meta.OwnerUser},
			want:  system,
		},
		{
			name:  "admin token may set any owner id",
			ctx:   adminCtx,
			owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-2"},
			want:  meta.Owner{Kind: meta.OwnerUser, ID: "u-2"},
		},
		{
			name:  "non-user owner kinds untouched",
			ctx:   userCtx,
			owner: meta.Owner{Kind: meta.OwnerHost, ID: "h-1"},
			want:  meta.Owner{Kind: meta.OwnerHost, ID: "h-1"},
		},
		{
			name:  "no actor without an owner id creates a system row",
			ctx:   context.Background(),
			owner: meta.Owner{Kind: meta.OwnerUser},
			want:  system,
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
			if o != tt.want {
				t.Fatalf("owner = %+v, want %+v", o, tt.want)
			}
		})
	}
}
