package rbac

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/authz"
)

func TestCheckGrant(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(map[string][]Binding{
		"user:editor": {{RoleID: "editor", Scope: org1}},
		"user:owner":  {{RoleID: "owner", Scope: folder1}},
	})
	kinds := []string{Wildcard, "documents", "folders"}
	verbs := []string{Wildcard, "get", "list", "delete"}
	editor := user("e", "user:editor")
	owner := user("o", "user:owner")

	tests := []struct {
		name         string
		p            *Principal
		role         Role
		scope        Scope
		kinds, verbs []string
		want         error
	}{
		{"holds every permission there", editor, roles["reader"], folder1, kinds, verbs, nil},
		{"holds them further out", editor, roles["editor"], org1, kinds, verbs, nil},
		{"not at a scope outside", editor, roles["reader"], global, kinds, verbs, authz.ErrForbidden},
		{"a wildcard expands to the vocabulary", editor, roles["anyget"], folder1, []string{"documents", "folders"}, verbs, nil},
		{"the vocabulary can exceed what is held", editor, roles["anyget"], folder1, []string{"documents", "widgets"}, verbs, authz.ErrForbidden},
		{"no vocabulary: wildcard checked as itself", owner, roles["owner"], folder1, nil, nil, nil},
		{"no vocabulary: a wildcard is not held", editor, roles["anyget"], folder1, nil, nil, authz.ErrForbidden},
		{"admin", &Principal{Admin: true}, roles["owner"], global, kinds, verbs, nil},
		{"unauthenticated", nil, roles["reader"], folder1, kinds, verbs, authz.ErrUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := e.CheckGrant(ctx, tt.p, tt.role, tt.scope, tt.kinds, tt.verbs)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("CheckGrant = %v, want %v", err, tt.want)
			}
			if errors.Is(err, authz.ErrForbidden) && !strings.Contains(err.Error(), tt.role.ID) {
				t.Fatalf("refusal %q does not name the role", err)
			}
		})
	}

	e.Scopes = folderChains{err: errDown}
	if err := e.CheckGrant(ctx, editor, roles["reader"], folder1, kinds, verbs); err != errDown {
		t.Fatalf("lookup failure = %v, want it unwrapped", err)
	}
}

func TestEachPermission(t *testing.T) {
	rules := []Rule{
		{Kinds: []string{"documents"}, Verbs: []string{Wildcard}},
		{Kinds: []string{Wildcard, "folders"}, Verbs: []string{"get"}},
	}
	var got []string
	err := EachPermission(rules, []string{Wildcard, "documents", "folders"}, []string{"get", Wildcard, "list"},
		func(kind, verb string) error {
			got = append(got, kind+"."+verb)
			return nil
		})
	want := []string{"documents.get", "documents.list", "documents.get", "folders.get"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("EachPermission = %v, %v; want %v", got, err, want)
	}

	got = nil
	err = EachPermission(rules, nil, nil, func(kind, verb string) error {
		got = append(got, kind+"."+verb)
		return errDown
	})
	if err != errDown || !reflect.DeepEqual(got, []string{"documents.*"}) {
		t.Fatalf("EachPermission did not stop at the first error: %v, %v", got, err)
	}
}
