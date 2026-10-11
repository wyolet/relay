package actor

import (
	"reflect"
	"testing"

	"github.com/wyolet/relay/auth/principal"
)

func TestPrincipalConversion(t *testing.T) {
	session := &principal.Principal{
		ID: "u-1", Kind: principal.KindUser, Username: "alice", SessionID: "sid",
		Roles: []string{"admin"}, Subjects: []string{"user:u-1"}, Groups: []string{"eng"},
		Credential: principal.Credential{Kind: principal.CredentialSession},
	}
	admin := &principal.Principal{Kind: principal.KindAdmin, Admin: true, Username: "admin-token"}

	tests := []struct {
		name  string
		p     *principal.Principal
		actor Actor
	}{
		{"session user", session, Actor{UserID: "u-1", Username: "alice", SessionID: "sid", Roles: []string{"admin"}, Subjects: []string{"user:u-1"}, IdPGroups: []string{"eng"}}},
		{"admin token", admin, Actor{Username: "admin-token", AdminToken: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := FromPrincipal(tt.p)
			if !reflect.DeepEqual(*a, tt.actor) {
				t.Fatalf("FromPrincipal = %+v, want %+v", *a, tt.actor)
			}
			if !a.IsAuthenticated() || !tt.p.Authenticated() {
				t.Errorf("authenticated: actor %v, principal %v; want both", a.IsAuthenticated(), tt.p.Authenticated())
			}
			back := a.Principal()
			want := *tt.p
			want.Credential = principal.Credential{}
			if !reflect.DeepEqual(back, want) {
				t.Errorf("Principal = %+v, want %+v", back, want)
			}
		})
	}
}
