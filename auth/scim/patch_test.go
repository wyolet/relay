package scim

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// The request bodies below are the ones Microsoft and Okta document their clients sending: learn.microsoft.com/entra/identity/app-provisioning/application-provisioning-config-problem-scim-compatibility and developer.okta.com/docs/api/openapi/okta-scim/guides/scim-20.

func TestEntraDisablesWithTheStringFalse(t *testing.T) {
	f := newFixture(t)
	id := f.createUser("ada@example.com", "obj-1")
	f.do(http.MethodPatch, "/Users/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "Replace",
          "path": "active",
          "value": "False"
      }
  ]
}`).want(t, http.StatusOK)
	if !f.user(id).Disabled || !slices.Contains(f.ended.users, id) {
		t.Fatalf("user not disabled, or its sessions not ended: %v", f.ended.users)
	}
	f.do(http.MethodPatch, "/Users/"+id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"Replace","path":"active","value":"True"}]}`).want(t, http.StatusOK)
	if f.user(id).Disabled {
		t.Fatal(`"True" did not re-enable the user`)
	}
}

func TestEntraDisablesWithABoolean(t *testing.T) {
	f := newFixture(t)
	id := f.createUser("ada@example.com", "obj-1")
	res := f.do(http.MethodPatch, "/Users/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "replace",
          "path": "active",
          "value": false
      }
  ]
}`).want(t, http.StatusOK)
	if res.body["active"] != false || !f.user(id).Disabled {
		t.Fatalf("response active = %v", res.body["active"])
	}
}

func TestEntraReplacesSeveralAttributesByPath(t *testing.T) {
	f := newFixture(t)
	id := f.createUser("ada@example.com", "obj-1")
	f.do(http.MethodPatch, "/Users/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "Replace",
          "path": "displayName",
          "value": "Pvlo"
      },
      {
          "op": "Replace",
          "path": "emails[type eq \"work\"].value",
          "value": "TestBcwqnm@test.microsoft.com"
      },
      {
          "op": "Replace",
          "path": "name.givenName",
          "value": "Gtfd"
      },
      {
          "op": "Replace",
          "path": "name.familyName",
          "value": "Pkqf"
      },
      {
          "op": "Replace",
          "path": "externalId",
          "value": "Eqpj"
      },
      {
          "op": "Replace",
          "path": "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:employeeNumber",
          "value": "Eqpj"
      }
  ]
}`).want(t, http.StatusOK)
	u := f.user(id)
	if u.Email != "TestBcwqnm@test.microsoft.com" || u.External.ID != "Eqpj" || u.Username != "ada@example.com" {
		t.Fatalf("user after patch: %+v", u)
	}
}

func TestEntraReplacesWithNoPathAndAValueObject(t *testing.T) {
	f := newFixture(t)
	id := f.createUser("ada@example.com", "obj-1")
	f.do(http.MethodPatch, "/Users/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "replace",
          "path": "emails[type eq \"work\"].value",
          "value": "TestMhvaes@test.microsoft.com"
      },
      {
          "op": "replace",
          "value": {
              "displayName": "Bjfe",
              "name.givenName": "Kkom",
              "name.familyName": "Unua",
              "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:employeeNumber": "Aklq"
          }
      }
  ]
}`).want(t, http.StatusOK)
	if u := f.user(id); u.Email != "TestMhvaes@test.microsoft.com" {
		t.Fatalf("email %q", u.Email)
	}
	// The same form carrying stored attributes, the core schema's URN among the keys.
	f.do(http.MethodPatch, "/Users/"+id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"replace","value":{"active":false,"urn:ietf:params:scim:schemas:core:2.0:User":{"userName":"ada2@example.com"}}}]}`).
		want(t, http.StatusOK)
	if u := f.user(id); !u.Disabled || u.Username != "ada2@example.com" {
		t.Fatalf("after pathless replace: %+v", u)
	}
}

func TestEntraAddsASingleValueAttributeWithACapitalisedOp(t *testing.T) {
	f := newFixture(t)
	id := f.createUser("ada@example.com", "obj-1")
	// nickName is a core attribute the store does not hold: accepted, not kept.
	f.do(http.MethodPatch, "/Users/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
    {
      "op": "Add",
      "path": "nickName",
      "value": "Babs"
    }
  ]
}`).want(t, http.StatusOK)
}

func TestEntraRemovesAMemberWithAValueArray(t *testing.T) {
	f := newFixture(t)
	u1, u2 := f.createUser("a@example.com", "a"), f.createUser("b@example.com", "b")
	gid := f.createGroup("Ops", u1, u2)
	f.do(http.MethodPatch, "/Groups/"+gid, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "Remove",
          "path": "members",
          "value": [
              {
                  "value": "`+u1+`"
              }
          ]
      }
  ]
}`).want(t, http.StatusNoContent)
	if got := f.group(gid).Spec.MemberIDs; !slices.Equal(got, []string{u2}) {
		t.Fatalf("members %v, want only %s", got, u2)
	}
}

func TestEntraRemovesAMemberWithAValueFilter(t *testing.T) {
	f := newFixture(t)
	u1, u2 := f.createUser("a@example.com", "a"), f.createUser("b@example.com", "b")
	gid := f.createGroup("Ops", u1, u2)
	f.do(http.MethodPatch, "/Groups/"+gid, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "remove",
          "path": "members[value eq \"`+u2+`\"]"
      }
  ]
}`).want(t, http.StatusNoContent)
	if got := f.group(gid).Spec.MemberIDs; !slices.Equal(got, []string{u1}) {
		t.Fatalf("members %v, want only %s", got, u1)
	}
}

func TestEntraAddsMembers(t *testing.T) {
	f := newFixture(t)
	u1 := f.createUser("a@example.com", "a")
	gid := f.createGroup("Ops")
	body := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Add","path":"members","value":[{"$ref":null,"value":"` + u1 + `"}]}]}`
	f.do(http.MethodPatch, "/Groups/"+gid, body).want(t, http.StatusNoContent)
	f.do(http.MethodPatch, "/Groups/"+gid, body).want(t, http.StatusNoContent) // adding twice is one member
	if got := f.group(gid).Spec.MemberIDs; !slices.Equal(got, []string{u1}) {
		t.Fatalf("members %v", got)
	}
}

func TestOktaDeactivatesWithNoPath(t *testing.T) {
	f := newFixture(t)
	id := f.createUser("test.user@okta.local", "00ujl29u0le5T6Aj10h7")
	f.do(http.MethodPatch, "/Users/"+id, `{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [{
        "op": "replace",
        "value": {
            "active": false
        }
    }]
}`).want(t, http.StatusOK)
	if !f.user(id).Disabled {
		t.Fatal("user not disabled")
	}
}

func TestOktaRenamesAGroupEchoingItsID(t *testing.T) {
	f := newFixture(t)
	gid := f.createGroup("Test SCIMv2")
	f.do(http.MethodPatch, "/Groups/"+gid, `{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [{
        "op": "replace",
        "value": {
            "id": "`+gid+`",
            "displayName": "Test SCIMv2 renamed"
        }
    }]
}`).want(t, http.StatusNoContent)
	g := f.group(gid)
	if g.Meta.DisplayName != "Test SCIMv2 renamed" || g.Meta.Name != "test-scimv2" {
		t.Fatalf("group after rename: %+v", g.Meta)
	}
}

func TestOktaRemovesAndAddsMembersInOneRequest(t *testing.T) {
	f := newFixture(t)
	u1, u2 := f.createUser("old@okta.local", "00u1"), f.createUser("test.user@okta.local", "00u2")
	gid := f.createGroup("Test SCIMv2", u1)
	f.do(http.MethodPatch, "/Groups/"+gid, `{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [
        {
            "op": "remove",
            "path": "members[value eq \"`+u1+`\"]"
        },
        {
            "op": "add",
            "path": "members",
            "value": [
                {
                    "value": "`+u2+`",
                    "display": "test.user@okta.local"
                }
            ]
        }
    ]
}`).want(t, http.StatusNoContent)
	if got := f.group(gid).Spec.MemberIDs; !slices.Equal(got, []string{u2}) {
		t.Fatalf("members %v", got)
	}
	// Okta's full push replaces the member list.
	f.do(http.MethodPatch, "/Groups/"+gid, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"replace","path":"members","value":[{"value":"`+u1+`","display":"old@okta.local"},{"value":"`+u2+`"}]}]}`).
		want(t, http.StatusNoContent)
	if got := f.group(gid).Spec.MemberIDs; !slices.Equal(got, []string{u1, u2}) {
		t.Fatalf("members after the full push %v", got)
	}
}

func TestPatchRefusesWhatItCannotApply(t *testing.T) {
	f := newFixture(t)
	id := f.createUser("ada@example.com", "obj-1")
	gid := f.createGroup("Ops")
	const op = `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[%s]}`
	for _, c := range []struct {
		path, ops, scimType string
	}{
		{"/Users/" + id, `{"op":"remove"}`, typeNoTarget},
		{"/Users/" + id, `{"op":"move","path":"userName","value":"x"}`, typeInvalidSyntax},
		{"/Users/" + id, `{"op":"replace","path":"userName"}`, typeInvalidValue},
		{"/Users/" + id, `{"op":"remove","path":"userName"}`, typeInvalidValue},
		{"/Users/" + id, `{"op":"replace","path":"active","value":"maybe"}`, typeInvalidValue},
		{"/Users/" + id, `{"op":"add","path":"groups","value":[{"value":"` + gid + `"}]}`, typeMutability},
		{"/Users/" + id, `{"op":"replace","path":"favouriteColour","value":"red"}`, typeInvalidPath},
		{"/Users/" + id, `{"op":"replace","path":"emails[type eq \"work\"","value":"x"}`, typeInvalidPath},
		{"/Users/" + id, `{"op":"replace","value":"not an object"}`, typeInvalidValue},
		{"/Groups/" + gid, `{"op":"add","path":"members[value eq \"x\"]","value":[{"value":"y"}]}`, typeInvalidPath},
		{"/Groups/" + gid, `{"op":"add","path":"members","value":[{"display":"no value"}]}`, typeInvalidValue},
		{"/Groups/" + gid, `{"op":"replace","path":"displayName","value":""}`, typeInvalidValue},
	} {
		res := f.do(http.MethodPatch, c.path, fmt.Sprintf(op, c.ops))
		if res.status != http.StatusBadRequest || res.str("scimType") != c.scimType {
			t.Errorf("%s: %d %s, want 400 %s", c.ops, res.status, res.str("scimType"), c.scimType)
		}
	}
	if res := f.do(http.MethodPatch, "/Users/"+id, `{"Operations":[{"op":"replace","path":"userName","value":"x"}]}`); res.status != http.StatusBadRequest {
		t.Errorf("a body without the PatchOp schema: %d", res.status)
	}
	// A failed operation applies nothing, the ones before it included.
	f.do(http.MethodPatch, "/Users/"+id, fmt.Sprintf(op, `{"op":"replace","path":"userName","value":"zed"},{"op":"replace","path":"active","value":"maybe"}`)).
		want(t, http.StatusBadRequest)
	if u := f.user(id); u.Username != "ada@example.com" {
		t.Fatalf("a refused patch wrote %q", u.Username)
	}
}
