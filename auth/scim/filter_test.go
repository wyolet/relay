package scim

import (
	"errors"
	"testing"
)

func TestFilterMatchesUsers(t *testing.T) {
	ada := map[string]any{"id": "u-1", "username": "Ada@Example.com", "externalid": "obj-1", "active": true,
		"emails": []map[string]any{{"value": "ada@example.com", "type": "work", "primary": true}}}
	bob := map[string]any{"id": "u-2", "username": "bob", "active": false}
	cases := []struct {
		filter   string
		ada, bob bool
	}{
		{`userName eq "ada@example.com"`, true, false}, // userName is not case-exact
		{`USERNAME EQ "ada@example.com"`, true, false}, // nor are names and operators
		{`urn:ietf:params:scim:schemas:core:2.0:User:userName eq "bob"`, false, true},
		{`externalId eq "obj-1"`, true, false},
		{`externalId eq "OBJ-1"`, false, false}, // externalId is case-exact
		{`id eq "u-2"`, false, true},
		{`userName ne "bob"`, true, false},
		{`userName co "example"`, true, false},
		{`userName sw "ada"`, true, false},
		{`userName ew ".com"`, true, false},
		{`userName gt "b"`, false, true},
		{`userName le "b"`, true, false},
		{`externalId pr`, true, false},
		{`emails pr`, true, false},
		{`emails.value eq "ada@example.com"`, true, false},
		{`emails eq "ADA@example.com"`, true, false}, // a multi-valued attribute alone means its value
		{`emails[type eq "work" and value co "@example"]`, true, false},
		{`emails[primary eq true]`, true, false},
		{`emails.primary eq false`, false, false},
		{`userName eq "bob" or externalId eq "obj-1"`, true, true},
		{`userName eq "bob" and externalId eq "obj-1"`, false, false},
		{`not (userName eq "bob")`, true, false},
		{`not(userName eq "bob")`, true, false},
		{`(userName eq "bob" or userName sw "ada") and emails pr`, true, false},
		// and binds tighter than or.
		{`userName eq "bob" or userName sw "ada" and externalId eq "x"`, false, true},
		{`userName eq "with \"quote\""`, false, false},
	}
	for _, c := range cases {
		e, err := parseFilter(c.filter, userSchema)
		if err != nil {
			t.Errorf("%s: %v", c.filter, err)
			continue
		}
		if got := e.eval(ada); got != c.ada {
			t.Errorf("%s on ada = %v, want %v", c.filter, got, c.ada)
		}
		if got := e.eval(bob); got != c.bob {
			t.Errorf("%s on bob = %v, want %v", c.filter, got, c.bob)
		}
	}
}

func TestFilterMatchesGroups(t *testing.T) {
	g := map[string]any{"id": "g-1", "displayname": "Engineering", "externalid": "x",
		"members": []map[string]any{{"value": "u-1"}, {"value": "u-2"}}}
	for filter, want := range map[string]bool{
		`displayName eq "engineering"`:                             true,
		`members[value eq "u-2"]`:                                  true,
		`members.value eq "u-3"`:                                   false,
		`members eq "u-1"`:                                         true,
		`members[value eq "u-3" or value sw "u-1"]`:                true,
		`displayName eq "Engineering" and members[value eq "U-1"]`: false, // member ids are case-exact
	} {
		e, err := parseFilter(filter, groupSchema)
		if err != nil {
			t.Errorf("%s: %v", filter, err)
			continue
		}
		if got := e.eval(g); got != want {
			t.Errorf("%s = %v, want %v", filter, got, want)
		}
	}
}

func TestFilterRefusesWhatItCannotEvaluate(t *testing.T) {
	for _, filter := range []string{
		`name.givenName eq "Ada"`, // not stored
		`displayName eq "Ada"`,    // a user's displayName is not stored
		`meta.lastModified gt "2011-05-13T04:42:34Z"`,
		`urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:employeeNumber eq "1"`,
		`active eq true`, // not one of the filterable attributes
		`userName eq 5`,  // numbers name no filterable attribute
		`userName eq null`,
		`userName eq true`,
		`emails.primary co "x"`,
		`userName`,
		`userName xx "a"`,
		`userName eq "a" and`,
		`(userName eq "a"`,
		`userName eq "a")`,
		`not userName eq "a"`,
		`userName eq "unterminated`,
		`emails[type eq "work"`,
		`emails[type eq "work"].value eq "x"`,
		`userName[value eq "x"]`,
		`emails[emails[value eq "x"]]`,
		`emails[nope eq "x"]`,
		``,
	} {
		_, err := parseFilter(filter, userSchema)
		if e, ok := errors.AsType[*Error](err); !ok || e.Type != typeInvalidFilter || e.Status != 400 {
			t.Errorf("%q: err = %v, want a 400 invalidFilter", filter, err)
		}
	}
	if _, err := parseFilter(`members.display eq "x"`, groupSchema); err == nil {
		t.Error("members.display accepted")
	}
}

func TestFilterBoundsItsWork(t *testing.T) {
	deep := ""
	for range maxFilterDepth + 2 {
		deep += "("
	}
	deep += `userName eq "a"`
	for range maxFilterDepth + 2 {
		deep += ")"
	}
	if _, err := parseFilter(deep, userSchema); err == nil {
		t.Error("a filter nested past the bound was accepted")
	}
	long := `userName eq "` + string(make([]byte, maxFilterLen)) + `"`
	if _, err := parseFilter(long, userSchema); err == nil {
		t.Error("a filter past the length bound was accepted")
	}
}
