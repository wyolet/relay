package control

import (
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
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

// A PUT without a value edits the row and keeps the stored secret; only an
// explicit value replaces it.
func TestHostKeyUpdateWithoutValueKeepsTheStoredSecret(t *testing.T) {
	for _, kind := range []hostkey.ValueKind{hostkey.ValueKindStored, hostkey.ValueKindOAuth} {
		t.Run(string(kind), func(t *testing.T) {
			existing := &hostkey.HostKey{Spec: hostkey.Spec{ValueFrom: hostkey.ValueFrom{Kind: kind, Provider: "acme"}}, Resolved: "sk-stored"}
			incoming := &hostkey.HostKey{Spec: existing.Spec}
			mergeHostKeyPreserveValue(existing, incoming)
			if incoming.Spec.Value != "sk-stored" && incoming.Resolved != "sk-stored" {
				t.Fatalf("value=%q resolved=%q, want the stored secret carried", incoming.Spec.Value, incoming.Resolved)
			}

			replaced := &hostkey.HostKey{Spec: existing.Spec}
			replaced.Spec.Value = "sk-new"
			mergeHostKeyPreserveValue(existing, replaced)
			if replaced.Spec.Value != "sk-new" {
				t.Fatalf("an explicit value was overwritten with %q", replaced.Spec.Value)
			}
		})
	}
}

// newRoleBindingsHarness mounts the real role-binding kind, whose owner
// mirrors spec.scope rather than defaulting to one kind.
func newRoleBindingsHarness(t *testing.T) http.Handler {
	t.Helper()
	rbmeta := func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta }
	store := &memStore[rolebinding.RoleBinding]{metaOf: rbmeta, items: map[string]*rolebinding.RoleBinding{}}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("rolebindings-test", "0"))
	registerKind[rolebinding.RoleBinding](
		api, "role-bindings", "role-binding", store, authz.AlwaysAllowAuthenticated{}, rbmeta,
		func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() },
		"",
		listScanResolver[rolebinding.RoleBinding](store, rbmeta),
		nil, nil, nil, nil, nil,
		noSettings{},
		false,
		nil,
		nil,
	)
	return r
}

// The global scope is spelled {kind: system}, and a role binding's owner
// mirrors its scope — so the reserved-owner guard must not reject the one
// shape a global binding can have.
func TestCreateGlobalRoleBindingIsNotReservedOwner(t *testing.T) {
	h := newRoleBindingsHarness(t)
	body := `{"metadata":{"name":"ops-admin","displayName":"Ops Admin","owner":{"kind":"system"}},` +
		`"spec":{"roleId":"00000000-0000-7000-8000-000000000001",` +
		`"scope":{"kind":"system"},"subjects":[{"kind":"user","id":"00000000-0000-7000-8000-000000000002"}]}}`
	w := scopeReq(t, h, "root", http.MethodPost, "/role-bindings", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
}
