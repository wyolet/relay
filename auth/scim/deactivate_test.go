package scim

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/wyolet/relay/auth/directory"
)

// A product's rule refuses a client's disable, deactivation or delete of an account it must keep, such as the last enabled admin: the client gets a 409, nothing is written, and the refusal is reported for the audit log.
func TestCheckDeactivateRefusesTheWrite(t *testing.T) {
	var protected string
	refuse := func(c *Config) {
		c.CheckDeactivate = func(_ context.Context, u *directory.User) error {
			if u.ID == protected {
				return fmt.Errorf("%w: this is the last enabled admin", ErrRefused)
			}
			return nil
		}
	}
	patch := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false},{"op":"replace","path":"emails[type eq \"work\"].value","value":"new@example.com"}]}`
	put := func(userName string) string {
		return `{"schemas":["` + schemaUser + `"],"userName":"` + userName + `","active":false,"emails":[{"primary":true,"value":"new-` + userName + `"}]}`
	}
	for name, tc := range map[string]struct {
		deleteDisables bool
		method         string
		body           func(userName string) string
	}{
		"patch active false": {method: http.MethodPatch, body: func(string) string { return patch }},
		"put active false":   {method: http.MethodPut, body: put},
		"delete":             {method: http.MethodDelete, body: func(string) string { return "" }},
		"delete disables":    {method: http.MethodDelete, deleteDisables: true, body: func(string) string { return "" }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, refuse, func(c *Config) { c.DeleteDisables = tc.deleteDisables })
			id := f.createUser("ada@example.com", "obj-1")
			protected = id
			f.writes = nil
			res := f.do(tc.method, "/Users/"+id, tc.body("ada@example.com")).want(t, http.StatusConflict)
			if res.str("detail") != "scim: refused: this is the last enabled admin" {
				t.Fatalf("detail = %q", res.str("detail"))
			}
			u := f.user(id)
			if u.Disabled || u.Email != "ada@example.com" {
				t.Fatalf("a refused write changed the account: %+v", u)
			}
			if len(f.writes) != 1 || !errors.Is(f.writes[0].Err, ErrRefused) || f.writes[0].ID != id {
				t.Fatalf("writes reported = %+v, want the refusal", f.writes)
			}

			other := f.createUser("bob@example.com", "obj-2")
			f.do(tc.method, "/Users/"+other, tc.body("bob@example.com")).want(t, map[bool]int{true: http.StatusOK, false: http.StatusNoContent}[tc.method != http.MethodDelete])
		})
	}
}
