package adapter

import "testing"

func TestPathSegment(t *testing.T) {
	for in, want := range map[string]string{
		"gemini-2.5-flash":        "gemini-2.5-flash",
		"m:tag@v1":                "m:tag@v1",
		"a/../b?x=1":              "a%2F..%2Fb%3Fx=1",
		"gemini-2.5-flash[1m]":    "gemini-2.5-flash%5B1m%5D",
		".":                       "%2E",
		"..":                      "%2E%2E",
		"with space#and-fragment": "with%20space%23and-fragment",
	} {
		if got := pathSegment(in); got != want {
			t.Errorf("pathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
