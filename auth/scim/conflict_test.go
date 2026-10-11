package scim

import (
	"net/http"
	"testing"
)

// A uniqueness conflict says only that: the taken value belongs to another account, which may be one the client cannot see.
func TestConflictNamesNoOtherAccount(t *testing.T) {
	f := newFixture(t)
	f.client("okta")
	f.as("okta", http.MethodPost, "/Users", `{"userName":"root@corp.example","emails":[{"primary":true,"value":"root@corp.example"}],"externalId":"o-1"}`).
		want(t, http.StatusCreated)
	for name, body := range map[string]string{
		"username": `{"userName":"root@corp.example"}`,
		"email":    `{"userName":"someone@corp.example","emails":[{"primary":true,"value":"root@corp.example"}]}`,
	} {
		res := f.do(http.MethodPost, "/Users", body).want(t, http.StatusConflict)
		if res.str("scimType") != typeUniqueness || res.str("detail") != "uniqueness conflict" {
			t.Fatalf("%s conflict = %s", name, res.raw)
		}
	}
}
