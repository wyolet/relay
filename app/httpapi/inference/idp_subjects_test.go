package inference

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/crypto"
)

// A token's groups come from the claims it was minted with — what the IdP
// asserted at login — unioned with the local membership the snapshot holds
// now. An IdP group that is not a local Group row must still resolve a
// binding, or SSO-only tenancy grants nothing.
func TestIdPAssertedGroupResolvesABinding(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-idp", 10, f.boundPol.Meta.ID, "group:sso-platform"),
	}
	st := f.stack(t)

	// Without the claim there is no local group of that name, so nothing
	// binds and the request is refused.
	if w := st.do(f.mint(t, nil)); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 with no matching subject", w.Code)
	}

	tok := f.mint(t, func(c *crypto.TokenClaims) { c.Grp = []string{"sso-platform"} })
	if w := st.do(tok); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !hasSubject(st.seen.Subjects, "group:sso-platform") {
		t.Fatalf("subjects = %v, want the IdP-asserted group", st.seen.Subjects)
	}
	if st.seen.Policy == nil || st.seen.Policy.Meta.ID != f.boundPol.Meta.ID {
		t.Fatalf("policy = %v, want the bound one", st.seen.PolicyID())
	}
}

// The union is deduplicated: a group asserted by the IdP that is also a
// local membership appears once.
func TestIdPAndLocalGroupsAreUnionedWithoutDuplicates(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated"),
	}
	st := f.stack(t)
	tok := f.mint(t, func(c *crypto.TokenClaims) { c.Grp = []string{"data-science", "sso-only"} })
	if w := st.do(tok); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	seen := map[string]int{}
	for _, s := range st.seen.Subjects {
		seen[s]++
	}
	if seen["group:data-science"] != 1 {
		t.Fatalf("subjects = %v, want group:data-science exactly once", st.seen.Subjects)
	}
	if seen["group:sso-only"] != 1 {
		t.Fatalf("subjects = %v, want the IdP-only group carried through", st.seen.Subjects)
	}
}

func hasSubject(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
