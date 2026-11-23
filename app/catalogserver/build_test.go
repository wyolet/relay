package catalogserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const fixtureCatalogJSON = `{"version":"v0.1.0","generatedAt":"2026-07-01T15:07:04Z",` +
	`"hosts":[{"name":"acme-api","baseURL":"https://api.example.com","models":[{"upstream":"rocket-1","model":"rocket-1","adapter":"openai","providers":["acme"]}],"displayName":"Acme API"}],` +
	`"models":[{"metadataName":"rocket-1","provider":"acme","displayName":"Rocket","capabilities":{},"modalities":{}}],` +
	`"providers":[{"name":"acme","displayName":"Acme"}]}`

func TestBuild_DataTreeWithoutDraftsAndCatalogOfIt(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", true)

	repo := Repo{Dir: src.dir}
	commit, at, err := repo.Commit(context.Background(), "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	// The committer's +05:00 offset must survive, not just the instant.
	if got, want := at.Format("2006-01-02T15:04:05Z07:00"), "2026-07-01T20:07:04+05:00"; got != want {
		t.Fatalf("commit time %s, want %s", got, want)
	}
	data := must(BuildData(context.Background(), repo, commit))(t)
	wantFiles := []string{
		"data/hosts/acme-api/host.yaml",
		"data/providers/acme/models/rocket.yaml",
		"data/providers/acme/provider.yaml",
	}
	if diff := cmp.Diff(wantFiles, tarFiles(t, data)); diff != "" {
		t.Fatalf("data tarball files (-want +got):\n%s", diff)
	}
	if channel, err := ArchiveChannel(data); err != nil || channel != "v1alpha2" {
		t.Fatalf("channel %q, %v", channel, err)
	}
	again, err := BuildData(context.Background(), repo, commit)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("data tarball is not reproducible (err %v)", err)
	}
	cat := must(BuildCatalog(data, at, "v0.1.0"))(t)
	var compact bytes.Buffer
	if err := json.Compact(&compact, gunzip(t, cat)); err != nil || compact.String() != fixtureCatalogJSON {
		t.Fatalf("catalog (err %v):\n got %s\nwant %s", err, compact.String(), fixtureCatalogJSON)
	}
	again, err = BuildCatalog(data, at, "v0.1.0")
	if err != nil || !bytes.Equal(cat, again) {
		t.Fatalf("catalog is not reproducible (err %v)", err)
	}
}

func TestBuildCatalog_ModelIcon(t *testing.T) {
	modelIcon := func(tree map[string]string) string {
		t.Helper()
		src := newSourceRepo(t)
		src.commit(tree, oldCommit)
		src.tag("v0.1.0", true)
		repo := Repo{Dir: src.dir}
		commit, at, err := repo.Commit(context.Background(), "v0.1.0")
		if err != nil {
			t.Fatal(err)
		}
		data := must(BuildData(context.Background(), repo, commit))(t)
		var cat struct {
			Models []struct {
				Icon string `json:"icon"`
			} `json:"models"`
		}
		if err := json.Unmarshal(gunzip(t, must(BuildCatalog(data, at, "v0.1.0"))(t)), &cat); err != nil || len(cat.Models) != 1 {
			t.Fatalf("catalog models %+v, err %v", cat.Models, err)
		}
		return cat.Models[0].Icon
	}
	withIcons := func(family, own string) map[string]string {
		tree := catalogTree(fixtureAPIVersion, "Rocket")
		tree["data/providers/acme/provider.yaml"] += "  icon:\n    path: /provider/acme.png\n"
		tree["data/providers/acme/models/rocket.yaml"] += "  family: rocket\n" + own
		tree["data/families.yaml"] = "families:\n  - name: rocket\n" + family
		return tree
	}
	const familyIcon = "    icon:\n      path: /provider/rocket.png\n"
	const ownIcon = "  icon:\n    path: /provider/rocket-1.png\n"
	for _, tc := range []struct {
		name string
		tree map[string]string
		want string
	}{
		{"the model's own", withIcons(familyIcon, ownIcon), "/provider/rocket-1.png"},
		{"its family's", withIcons(familyIcon, ""), "/provider/rocket.png"},
		{"its provider's", withIcons("", ""), "/provider/acme.png"},
	} {
		if got := modelIcon(tc.tree); got != tc.want {
			t.Errorf("%s: icon %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseSumFile(t *testing.T) {
	const sum = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	if got := string(SumFile(CatalogAsset, []byte("x"))); got != sum+"  catalog.json.gz\n" {
		t.Fatalf("SumFile wrote %q, want the sha256sum line of x", got)
	}
	for _, c := range []struct {
		name, line, wantErr string
	}{
		{"sha256sum text mode", sum + "  catalog.json.gz\n", ""},
		{"sha256sum binary mode", sum + " *catalog.json.gz\n", ""},
		{"digest alone", sum + "\n", ""},
		{"upper-case digest", strings.ToUpper(sum) + "  catalog.json.gz\n", ""},
		{"names another file", sum + "  other.gz\n", `names "other.gz", want "catalog.json.gz"`},
		{"short digest", sum[:63] + "  catalog.json.gz\n", `invalid sha256 "` + sum[:63] + `"`},
		{"non-hex digest", "g" + sum[1:] + "\n", `invalid sha256 "g` + sum[1:] + `"`},
		{"empty", "", "not a sha256sum line"},
		{"two lines", sum + "  catalog.json.gz\n" + sum + "  x.gz\n", "not a sha256sum line"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSumFile([]byte(c.line), CatalogAsset)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr || got != "" {
					t.Fatalf("got %q, err %v; want err %q", got, err, c.wantErr)
				}
				return
			}
			if err != nil || got != sum {
				t.Fatalf("got %q, err %v; want %q", got, err, sum)
			}
		})
	}
}

func TestArchiveChannel_Refusals(t *testing.T) {
	src := newSourceRepo(t)
	host := catalogTree(fixtureAPIVersion, "Rocket")["data/hosts/acme-api/host.yaml"]
	for _, c := range []struct {
		name string
		tree map[string]string
		want string
	}{
		{"documents disagree", map[string]string{"data/hosts/acme-api/host.yaml": strings.Replace(host, "apiVersion: "+fixtureAPIVersion+"\nkind: HostBinding", "apiVersion: relay.wyolet.dev/v1beta1\nkind: HostBinding", 1)},
			`hosts/acme-api/host.yaml has apiVersion "relay.wyolet.dev/v1beta1", hosts/acme-api/host.yaml has "relay.wyolet.dev/v1alpha2"`},
		{"kind without apiVersion", map[string]string{"data/hosts/a.yaml": "kind: Host\n"}, "hosts/a.yaml: doc 0: no apiVersion"},
		{"only drafts and empty documents", map[string]string{"data/a.yaml": "---\n---\n", "data/b.draft.yaml": "apiVersion: x/v1\nkind: Host\n", "data/notes.txt": "apiVersion: x/v1\n"}, "no documents"},
		{"channel is not a path segment", map[string]string{"data/a.yaml": "apiVersion: relay.wyolet.dev/V1_Alpha\nkind: Host\n"}, `apiVersion "relay.wyolet.dev/V1_Alpha": invalid channel`},
		{"not YAML", map[string]string{"data/a.yml": "apiVersion: [\n"}, "a.yml: doc 0: yaml: line 1: did not find expected node content"},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := must(BuildData(context.Background(), Repo{Dir: src.dir}, src.commit(c.tree, oldCommit)))(t)
			ch, err := ArchiveChannel(data)
			if err == nil || err.Error() != c.want || ch != "" {
				t.Fatalf("channel %q, err %v; want err %q", ch, err, c.want)
			}
		})
	}
}
