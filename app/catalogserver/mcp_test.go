package catalogserver

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

const fixtureMCPAPIVersion = "gateway.example.com/v1alpha1"

// mcpCatalogTree is catalogTree plus an mcp/ tree with a draft beside it.
func mcpCatalogTree(dataAPIVersion, mcpAPIVersion, displayName string) map[string]string {
	tree := catalogTree(dataAPIVersion, displayName)
	tree["mcp/servers/wiki.yaml"] = `apiVersion: ` + mcpAPIVersion + `
kind: MCPTemplate
metadata:
  name: wiki
spec:
  title: ` + displayName + `
---
apiVersion: ` + mcpAPIVersion + `
kind: CredentialTemplate
metadata:
  name: wiki-token
`
	tree["mcp/drafts/servers/wip.yaml"] = "apiVersion: gateway.example.com/v0draft\nkind: MCPTemplate\n"
	return tree
}

// releaseMCP commits tree at at, tags it and publishes its three files.
func (f *fakeReleases) releaseMCP(src *sourceRepo, tag string, tree map[string]string, at time.Time) (data, catalog, mcp []byte) {
	src.t.Helper()
	commit := src.commit(tree, at)
	src.tag(tag, false)
	data, catalog = src.releaseFiles(tag)
	mcp = must(BuildMCP(context.Background(), Repo{Dir: src.dir}, commit))(src.t)
	f.publishBoth(tag, data, catalog)
	f.publish(tag, MCPAsset(tag), mcp, mcp)
	return data, catalog, mcp
}

func TestBuildMCP_TreeWithoutDraftsAndItsChannel(t *testing.T) {
	src := newSourceRepo(t)
	repo := Repo{Dir: src.dir}
	ctx := context.Background()

	old := src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	if b, err := BuildMCP(ctx, repo, old); !errors.Is(err, ErrNoMCPTree) || b != nil {
		t.Fatalf("tree without mcp/: %d bytes, err %v; want ErrNoMCPTree", len(b), err)
	}

	commit := src.commit(mcpCatalogTree(fixtureAPIVersion, fixtureMCPAPIVersion, "Wiki"), newCommit)
	mcp := must(BuildMCP(ctx, repo, commit))(t)
	if diff := cmp.Diff([]string{"mcp/servers/wiki.yaml"}, tarFiles(t, mcp)); diff != "" {
		t.Fatalf("mcp tarball files (-want +got):\n%s", diff)
	}
	if again := must(BuildMCP(ctx, repo, commit))(t); !bytes.Equal(mcp, again) {
		t.Fatal("mcp tarball is not reproducible")
	}
	if ch, err := MCPArchiveChannel(mcp); err != nil || ch != "v1alpha1" {
		t.Fatalf("mcp channel %q, %v", ch, err)
	}
	// The data tarball and its channel ignore mcp/.
	data := must(BuildData(ctx, repo, commit))(t)
	if diff := cmp.Diff([]string{
		"data/hosts/acme-api/host.yaml",
		"data/providers/acme/models/rocket.yaml",
		"data/providers/acme/provider.yaml",
	}, tarFiles(t, data)); diff != "" {
		t.Fatalf("data tarball files (-want +got):\n%s", diff)
	}
	if ch, err := ArchiveChannel(data); err != nil || ch != "v1alpha2" {
		t.Fatalf("data channel %q, %v", ch, err)
	}

	draftsOnly := catalogTree(fixtureAPIVersion, "Rocket")
	draftsOnly["mcp/drafts/servers/wip.yaml"] = "apiVersion: " + fixtureMCPAPIVersion + "\nkind: MCPTemplate\n"
	empty := must(BuildMCP(ctx, repo, src.commit(draftsOnly, newCommit)))(t)
	if ch, err := MCPArchiveChannel(empty); !errors.Is(err, ErrNoDocuments) || ch != "" {
		t.Fatalf("mcp/ with drafts only: channel %q, err %v; want ErrNoDocuments", ch, err)
	}
}

func TestServe_MCPAssetAndIndex(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	rel.release(src, "v0.1.0", "Rocket", oldCommit)
	_, _, mcp := rel.releaseMCP(src, "v0.1.1", mcpCatalogTree(fixtureAPIVersion, fixtureMCPAPIVersion, "Rocket 2"), newCommit)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	const index = "channels:\n    v1alpha2:\n        latest: v0.1.1\n"
	const mcpIndex = "channels:\n    v1alpha1:\n        latest: v0.1.1\n"
	etag := func(b string) string { return `"` + sha256Hex([]byte(b)) + `"` }
	sum := string(SumFile(MCPAsset("v0.1.1"), mcp))
	served := func(body, contentType, cacheControl string) answer {
		return answer{Code: http.StatusOK, Body: body, Header: map[string]string{
			"Content-Type": contentType, "Content-Length": strconv.Itoa(len(body)),
			"Cache-Control": cacheControl, "ETag": etag(body), "Location": "",
		}}
	}
	notModified := func(of answer) answer {
		of.Code, of.Body = http.StatusNotModified, ""
		of.Header["Content-Type"], of.Header["Content-Length"] = "", ""
		return of
	}
	redirect := func(to string) answer {
		return answer{Code: http.StatusFound, Body: "<a href=\"" + to + "\">Found</a>.\n\n", Header: map[string]string{
			"Cache-Control": shortCache, "Location": to,
		}}
	}
	notFound := func(cacheControl string) answer {
		return answer{Code: http.StatusNotFound, Body: "404 page not found\n", Header: map[string]string{"Cache-Control": cacheControl}}
	}
	for _, c := range []struct {
		name, path string
		request    []string
		want       answer
	}{
		{"relay index has no mcp channel", "/index.yaml", nil, served(index, "application/yaml", shortCache)},
		{"mcp index", "/mcp/index.yaml", nil, served(mcpIndex, "application/yaml", shortCache)},
		{"mcp index revalidated", "/mcp/index.yaml", []string{"If-None-Match", etag(mcpIndex)}, notModified(served(mcpIndex, "", shortCache))},
		{"mcp tarball", "/releases/v0.1.1/mcp-catalog-data-v0.1.1.tar.gz", []string{"Accept-Encoding", "gzip"}, served(string(mcp), "application/gzip", immutableCache)},
		{"mcp tarball revalidated", "/releases/v0.1.1/mcp-catalog-data-v0.1.1.tar.gz", []string{"If-None-Match", etag(string(mcp))}, notModified(served(string(mcp), "", immutableCache))},
		{"mcp sum", "/releases/v0.1.1/mcp-catalog-data-v0.1.1.tar.gz.sha256", nil, served(sum, "text/plain; charset=utf-8", immutableCache)},
		{"channel mcp", "/channels/v1alpha1/latest/mcp-catalog-data.tar.gz", nil, redirect("/releases/v0.1.1/mcp-catalog-data-v0.1.1.tar.gz")},
		{"channel mcp sum", "/channels/v1alpha1/latest/mcp-catalog-data.tar.gz.sha256", nil, redirect("/releases/v0.1.1/mcp-catalog-data-v0.1.1.tar.gz.sha256")},
		{"mcp file under a relay channel", "/channels/v1alpha2/latest/mcp-catalog-data.tar.gz", nil, notFound("")},
		{"relay file under an mcp channel", "/channels/v1alpha1/latest/catalog.json.gz", nil, notFound("")},
		{"tag without mcp/", "/releases/v0.1.0/mcp-catalog-data-v0.1.0.tar.gz", nil, notFound("no-store")},
		{"tag without mcp/, sum", "/releases/v0.1.0/mcp-catalog-data-v0.1.0.tar.gz.sha256", nil, notFound("no-store")},
		{"another tag's name", "/releases/v0.1.1/mcp-catalog-data-v0.1.0.tar.gz", nil, notFound("")},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantAnswer(t, "answer", c.want, get(t, s, c.path, c.request...))
		})
	}
	// A tree without mcp/ can never publish the tarball, so its sum is never asked for.
	if n := rel.asked["v0.1.0/"+MCPAsset("v0.1.0")+".sha256"]; n != 0 {
		t.Fatalf("v0.1.0's mcp sum asked for %d times", n)
	}
	if n := rel.fileHits("v0.1.1", MCPAsset("v0.1.1")); n != 0 {
		t.Fatalf("mcp tarball fetched from the release %d times; the rebuild matched", n)
	}
	if !s.cache.pinned[fileKey{"v0.1.1", assetMCP}] {
		t.Fatal("the mcp channel's latest tarball is not pinned")
	}
}

func TestIndex_MCPNeverHoldsBackRelayIndex(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	_, _, mcp0 := rel.releaseMCP(src, "v0.1.0", mcpCatalogTree(fixtureAPIVersion, fixtureMCPAPIVersion, "Rocket"), oldCommit)

	// v0.1.1's mcp tarball fails its sum; v0.1.2's mcp/ documents disagree on apiVersion.
	src.commit(mcpCatalogTree(fixtureAPIVersion, fixtureMCPAPIVersion, "Rocket 2"), newCommit)
	src.tag("v0.1.1", false)
	data1, cat1 := src.releaseFiles("v0.1.1")
	rel.publishBoth("v0.1.1", data1, cat1)
	rel.publish("v0.1.1", MCPAsset("v0.1.1"), gzipBytes(t, "tampered"), gzipBytes(t, "what CI built"))

	mixed := mcpCatalogTree(fixtureAPIVersion, fixtureMCPAPIVersion, "Rocket 3")
	mixed["mcp/servers/other.yaml"] = "apiVersion: gateway.example.com/v1beta1\nkind: MCPTemplate\n"
	_, _, mcp2 := rel.releaseMCP(src, "v0.1.2", mixed, newCommit.Add(time.Hour))

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	if diff := cmp.Diff(map[string]string{"v1alpha2": "v0.1.2"}, indexLatest(t, s)); diff != "" {
		t.Fatalf("relay index (-want +got):\n%s", diff)
	}
	s.mu.RLock()
	mcpLatest := maps.Clone(s.mcpLatest)
	s.mu.RUnlock()
	if diff := cmp.Diff(map[string]string{"v1alpha1": "v0.1.0"}, mcpLatest); diff != "" {
		t.Fatalf("mcp index (-want +got):\n%s", diff)
	}
	wantAnswer(t, "tampered mcp tarball", answer{Code: http.StatusServiceUnavailable, Body: "file unavailable\n"},
		get(t, s, "/releases/v0.1.1/"+MCPAsset("v0.1.1")))
	// Never indexed, but published and verified, so it serves like any tag's file.
	for tag, want := range map[string][]byte{"v0.1.0": mcp0, "v0.1.2": mcp2} {
		if rec := get(t, s, "/releases/"+tag+"/"+MCPAsset(tag)); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatalf("%s mcp tarball: %d, body equal %v", tag, rec.Code, bytes.Equal(rec.Body.Bytes(), want))
		}
	}
	wantCount(t, "mcp release failures", s.metrics.releaseFailures.WithLabelValues("mcp"), 1)
}

func TestHTTP_MCPIndexBeforeFirstSync(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	rel.release(src, "v0.1.0", "Rocket", oldCommit)
	s := newTestServer(t, src, rel, 64<<20)
	wantAnswer(t, "before the first sync", answer{Code: http.StatusServiceUnavailable, Body: "index not built yet\n"}, get(t, s, "/mcp/index.yaml"))
	s.sync(context.Background())
	wantAnswer(t, "no tag has mcp/", answer{Code: http.StatusOK, Body: "channels: {}\n"}, get(t, s, "/mcp/index.yaml"))
	wantAnswer(t, "readyz", answer{Code: http.StatusOK, Body: "ok\n"}, get(t, s, "/readyz"))
}
