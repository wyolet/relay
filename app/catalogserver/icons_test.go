package catalogserver

import (
	"context"
	"net/http"
	"testing"
)

func TestHTTP_ReleaseIcons(t *testing.T) {
	const icon = "\x89PNG acme"
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	tree := catalogTree(fixtureAPIVersion, "Rocket")
	tree["data/icons/acme.png"] = icon
	tree["data/icons/notes.txt"] = "not an icon"
	src.commit(tree, oldCommit)
	src.tag("v0.1.0", false)
	data, cat := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data, cat)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	served := answer{Code: http.StatusOK, Body: icon, Header: map[string]string{
		"Content-Type": "image/png", "Cache-Control": immutableCache, "ETag": `"` + sha256Hex([]byte(icon)) + `"`,
		"X-Content-Type-Options": "nosniff", "Access-Control-Allow-Origin": "*",
	}}
	wantAnswer(t, "icon", served, get(t, s, "/releases/v0.1.0/icons/acme.png"))

	redirect := get(t, s, "/channels/v1alpha2/latest/icons/acme.png")
	if redirect.Code != http.StatusFound || redirect.Header().Get("Location") != "/releases/v0.1.0/icons/acme.png" {
		t.Errorf("channel icon: %d to %q", redirect.Code, redirect.Header().Get("Location"))
	}

	for name, path := range map[string]string{
		"an icon the release lacks": "/releases/v0.1.0/icons/other.png",
		"a file that is no image":   "/releases/v0.1.0/icons/notes.txt",
		"a path outside icons":      "/releases/v0.1.0/icons/..%2Fproviders%2Facme%2Fprovider.yaml",
		"an unreleased tag":         "/releases/v9.9.9/icons/acme.png",
		"an unknown channel":        "/channels/v0/latest/icons/acme.png",
	} {
		if rec := get(t, s, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, rec.Code)
		}
	}
}
