package oauth

import "testing"

// The match is on the URL's hostname, exact and case-insensitive: no substring, userinfo or trailing-dot variant passes.
func TestCheckTokenHost(t *testing.T) {
	r := NewResolver(nil, nil, TokenHosts([]string{"Allowed.Example"}))
	for u, allowed := range map[string]bool{
		"https://allowed.example/token":              true,
		"https://ALLOWED.EXAMPLE:8443/token":         true,
		"https://allowed.example@evil.example/token": false,
		"https://allowed.example.evil.example/token": false,
		"https://allowed.example./token":             false,
		"https://evil.example/allowed.example":       false,
		"https://xallowed.example/token":             false,
		"::not a url":                                false,
	} {
		if got := r.checkTokenHost(u) == nil; got != allowed {
			t.Errorf("%s: allowed %v, want %v", u, got, allowed)
		}
	}
	if err := NewResolver(nil, nil).checkTokenHost("https://allowed.example/token"); err == nil {
		t.Error("a resolver with no listed host allowed one")
	}
}
