package scim

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestListPaginates(t *testing.T) {
	f := newFixture(t)
	for i := range 5 {
		f.createUser(fmt.Sprintf("user%d@example.com", i), fmt.Sprintf("x%d", i))
	}
	for _, c := range []struct {
		query                 string
		total, start, perPage int
		first                 string
	}{
		{"", 5, 1, 5, "user0@example.com"},
		{"startIndex=2&count=2", 5, 2, 2, "user1@example.com"},
		{"startIndex=5&count=10", 5, 5, 1, "user4@example.com"},
		{"startIndex=9", 5, 9, 0, ""},
		{"startIndex=0&count=1", 5, 1, 1, "user0@example.com"}, // below 1 reads as 1
		{"count=0", 5, 1, 0, ""},                               // totals only
		{"count=-3", 5, 1, 0, ""},
		{"count=100000", 5, 1, 5, "user0@example.com"}, // capped at maxCount
		{"filter=" + url.QueryEscape(`userName sw "user3"`), 1, 1, 1, "user3@example.com"},
	} {
		res := f.do(http.MethodGet, "/Users?"+c.query, "").want(t, http.StatusOK)
		rs, _ := res.body["Resources"].([]any)
		if num(res.body["totalResults"]) != c.total || num(res.body["startIndex"]) != c.start ||
			num(res.body["itemsPerPage"]) != c.perPage || len(rs) != c.perPage {
			t.Errorf("%q: %s", c.query, res.raw)
			continue
		}
		if c.first != "" && rs[0].(map[string]any)["userName"] != c.first {
			t.Errorf("%q: first %v, want %s", c.query, rs[0], c.first)
		}
		if res.body["schemas"].([]any)[0] != schemaListResponse {
			t.Errorf("%q: schemas %v", c.query, res.body["schemas"])
		}
	}
	for _, q := range []string{"count=ten", "startIndex=x"} {
		if res := f.do(http.MethodGet, "/Users?"+q, ""); res.status != 400 || res.str("scimType") != typeInvalidValue {
			t.Errorf("%q: %d %s", q, res.status, res.raw)
		}
	}
}

func num(v any) int { f, _ := v.(float64); return int(f) }

func TestErrorsUseTheSCIMFormat(t *testing.T) {
	f := newFixture(t)
	res := f.do(http.MethodGet, "/Users?filter="+url.QueryEscape(`title eq "x"`), "")
	if res.status != 400 || res.header.Get("Content-Type") != contentType {
		t.Fatalf("%d %s", res.status, res.header.Get("Content-Type"))
	}
	if s := res.body["schemas"].([]any); len(s) != 1 || s[0] != schemaError {
		t.Fatalf("schemas %v", s)
	}
	// RFC 7644 renders status as a string.
	if res.body["status"] != "400" || res.str("scimType") != typeInvalidFilter || res.str("detail") == "" {
		t.Fatalf("body %s", res.raw)
	}
	for _, c := range []struct {
		method, path, body string
		status             int
		scimType           string
	}{
		{http.MethodGet, "/Users/nope", "", 404, ""},
		{http.MethodGet, "/Nothing", "", 404, ""},
		{http.MethodPost, "/Bulk", "{}", 501, ""},
		{http.MethodDelete, "/Users", "", 405, ""},
		{http.MethodPost, "/Users", `{"userName":`, 400, typeInvalidSyntax},
		{http.MethodPost, "/Users", `{"emails":[]}`, 400, typeInvalidValue},
		{http.MethodPost, "/Groups", `{"members":[]}`, 400, typeInvalidValue},
	} {
		res := f.do(c.method, c.path, c.body)
		if res.status != c.status || res.str("scimType") != c.scimType || res.body["status"] != fmt.Sprint(c.status) {
			t.Errorf("%s %s: %s", c.method, c.path, res.raw)
		}
	}
	f.createUser("ada@example.com", "obj-1")
	if res := f.do(http.MethodPost, "/Users", `{"userName":"ada@example.com"}`); res.status != 409 || res.str("scimType") != typeUniqueness {
		t.Errorf("duplicate userName: %s", res.raw)
	}
}

func TestRequestsNeedALiveToken(t *testing.T) {
	f := newFixture(t)
	for name, auth := range map[string]string{
		"none":         "",
		"wrong scheme": "Basic " + f.tokenOf["entra"],
		"unknown":      "Bearer scim_not-a-real-token",
		"foreign":      "Bearer rk_something",
	} {
		req, _ := http.NewRequest(http.MethodGet, "/ServiceProviderConfig", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	f.do(http.MethodGet, "/ServiceProviderConfig", "").want(t, http.StatusOK)
	if f.tokens.used["entra"].IsZero() {
		t.Error("use was not recorded")
	}
	if err := f.tokens.RevokeClient(context.Background(), "entra"); err != nil {
		t.Fatal(err)
	}
	f.do(http.MethodGet, "/ServiceProviderConfig", "").want(t, http.StatusUnauthorized)
}

func TestCreateClientValidatesAndHidesTheToken(t *testing.T) {
	f := newFixture(t)
	if _, _, err := CreateClient(context.Background(), f.tokens, "Not A Label"); err == nil {
		t.Fatal("a name that is not a DNS label was accepted")
	}
	tok, c, err := CreateClient(context.Background(), f.tokens, "okta")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < len(TokenPrefix)+60 || tok[:len(TokenPrefix)] != TokenPrefix || c.TokenHash == "" || c.TokenHash == tok || c.TokenPrefix == tok {
		t.Fatalf("token %q client %+v", tok, c)
	}
	if _, _, err := CreateClient(context.Background(), f.tokens, "okta"); err != ErrClientExists {
		t.Fatalf("second create: %v", err)
	}
}

func TestUserLifecycle(t *testing.T) {
	f := newFixture(t)
	res := f.do(http.MethodPost, "/Users", `{
    "schemas": ["urn:ietf:params:scim:schemas:core:2.0:User","urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
    "externalId": "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef",
    "userName": "Test_User_ab6490ee-1e48-479e-a20b-2d77186b5dd1",
    "active": true,
    "emails": [{
        "primary": true,
        "type": "work",
        "value": "Test_User_fd0ea19b-0777-472c-9f96-4f70d2226f2e@testuser.com"
    }],
    "meta": {
        "resourceType": "User"
    },
    "name": {
        "formatted": "givenName familyName",
        "familyName": "familyName",
        "givenName": "givenName"
    },
    "roles": []
}`).want(t, http.StatusCreated)
	id := res.str("id")
	if res.header.Get("Location") != "https://idp.example/scim/v2/Users/"+id || res.body["active"] != true {
		t.Fatalf("create: %s %s", res.header.Get("Location"), res.raw)
	}
	q := "/Users?filter=" + url.QueryEscape(`userName eq "Test_User_ab6490ee-1e48-479e-a20b-2d77186b5dd1"`)
	if got := f.do(http.MethodGet, q, "").want(t, http.StatusOK); num(got.body["totalResults"]) != 1 {
		t.Fatalf("lookup by userName: %s", got.raw)
	}
	q = "/Users?filter=" + url.QueryEscape(`userName eq "non-existent user"`)
	if got := f.do(http.MethodGet, q, "").want(t, http.StatusOK); num(got.body["totalResults"]) != 0 {
		t.Fatalf("lookup of a missing user: %s", got.raw)
	}

	// PUT keeps what it leaves out: Okta's PUT carries no externalId.
	f.do(http.MethodPut, "/Users/"+id, `{"schemas":["`+schemaUser+`"],"id":"`+id+`","userName":"renamed","active":true,
		"emails":[{"primary":true,"type":"work","value":"renamed@example.com"}]}`).want(t, http.StatusOK)
	if u := f.user(id); u.Username != "renamed" || u.Email != "renamed@example.com" || u.External.ID != "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef" {
		t.Fatalf("after PUT %+v", u)
	}
	f.do(http.MethodDelete, "/Users/"+id, "").want(t, http.StatusNoContent)
	f.do(http.MethodGet, "/Users/"+id, "").want(t, http.StatusNotFound)
	ops := map[string]bool{}
	for _, w := range f.writes {
		ops[w.Op] = w.Err == nil && w.Client == "entra" && w.Kind == "user" && w.ID == id
	}
	if !ops["create"] || !ops["update"] || !ops["delete"] {
		t.Fatalf("writes %+v", f.writes)
	}
}

func TestDeleteCanDisableInstead(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.DeleteDisables = true })
	id := f.createUser("ada@example.com", "obj-1")
	f.do(http.MethodDelete, "/Users/"+id, "").want(t, http.StatusNoContent)
	if u := f.user(id); !u.Disabled {
		t.Fatal("DELETE did not disable")
	}
}

func TestGroupReadsAndSelection(t *testing.T) {
	f := newFixture(t)
	u := f.createUser("ada@example.com", "obj-1")
	gid := f.createGroup("Engineering", u)
	res := f.do(http.MethodGet, "/Groups?excludedAttributes=members&filter="+url.QueryEscape(`displayName eq "engineering"`), "").want(t, http.StatusOK)
	rs := res.body["Resources"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["members"] != nil || rs[0].(map[string]any)["id"] != gid {
		t.Fatalf("list: %s", res.raw)
	}
	g := f.do(http.MethodGet, "/Groups/"+gid+"?attributes=displayName", "").want(t, http.StatusOK)
	if g.body["displayName"] != "Engineering" || g.body["members"] != nil || g.body["meta"] != nil || g.body["id"] != gid {
		t.Fatalf("attributes=displayName: %s", g.raw)
	}
	usr := f.do(http.MethodGet, "/Users/"+u, "").want(t, http.StatusOK)
	if gs, _ := usr.body["groups"].([]any); len(gs) != 1 || gs[0].(map[string]any)["value"] != gid {
		t.Fatalf("user groups: %s", usr.raw)
	}
	f.do(http.MethodPut, "/Groups/"+gid, `{"schemas":["`+schemaGroup+`"],"displayName":"Eng"}`).want(t, http.StatusOK)
	if g := f.group(gid); g.Meta.DisplayName != "Eng" || len(g.Spec.MemberIDs) != 1 {
		t.Fatalf("PUT without members: %+v", g)
	}
	f.do(http.MethodDelete, "/Groups/"+gid, "").want(t, http.StatusNoContent)
	f.do(http.MethodDelete, "/Groups/"+gid, "").want(t, http.StatusNotFound)
}

func TestDiscoveryEndpoints(t *testing.T) {
	f := newFixture(t)
	sp := f.do(http.MethodGet, "/ServiceProviderConfig", "").want(t, http.StatusOK)
	if sp.body["patch"].(map[string]any)["supported"] != true || sp.body["bulk"].(map[string]any)["supported"] != false {
		t.Fatalf("config %s", sp.raw)
	}
	if rt := f.do(http.MethodGet, "/ResourceTypes", "").want(t, http.StatusOK); num(rt.body["totalResults"]) != 2 {
		t.Fatalf("resource types %s", rt.raw)
	}
	f.do(http.MethodGet, "/ResourceTypes/User", "").want(t, http.StatusOK)
	s := f.do(http.MethodGet, "/Schemas/"+schemaGroup, "").want(t, http.StatusOK)
	if s.body["name"] != "Group" {
		t.Fatalf("group schema %s", s.raw)
	}
	f.do(http.MethodGet, "/Schemas/urn:nope", "").want(t, http.StatusNotFound)
}
