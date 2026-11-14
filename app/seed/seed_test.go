package seed

import (
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

const envHostKeyTree = `apiVersion: relay.wyolet.dev/v1alpha3
kind: HostKey
metadata:
  name: tree-key
spec:
  hostId: tree-host
  valueFrom:
    kind: env
    env: TREE_UPSTREAM_KEY
`

// A catalog tree carries templates, not credentials: a HostKey in it would
// resolve a relay environment variable and send it to a host the same tree
// defines. The operator's own seed (`relay seed --apply`) still takes it.
func TestCatalogTreeRefusesHostKeys(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(envHostKeyTree))
	if err != nil {
		t.Fatal(err)
	}
	keep, _, refused := splitSeedDocs(slices.Clone(docs), true)
	if len(keep) != 0 || !slices.Equal(refused, []string{"HostKey/tree-key"}) {
		t.Fatalf("kept %d, refused %v; want the HostKey refused", len(keep), refused)
	}
	keep, _, refused = splitSeedDocs(slices.Clone(docs), false)
	if len(keep) != 1 || len(refused) != 0 {
		t.Fatalf("operator seed: kept %d, refused %v; want the HostKey kept", len(keep), refused)
	}
}

// A Setting in a seeded tree is dropped here (settings load from
// RELAY_SETTINGS_DIR), so the run has to name what it left out.
func TestDroppedSettingDocumentsAreNamed(t *testing.T) {
	docs := []manifest.Document{
		{Setting: &manifest.SettingDTO{Kind: "Setting", Metadata: manifest.WireMeta{Name: "auth:oidc"}}},
		{Provider: &manifest.ProviderDTO{Kind: "Provider", Metadata: manifest.WireMeta{Name: "acme"}}},
		{Setting: &manifest.SettingDTO{Kind: "Setting", Metadata: manifest.WireMeta{Name: "payload-logging"}}},
		{Team: &manifest.TeamDTO{Kind: "Team", Metadata: manifest.WireMeta{Name: "platform"}}},
	}

	keep, settings, tenancy := splitSeedDocs(docs, true)
	want := []string{"Setting/auth:oidc", "Setting/payload-logging"}
	if !slices.Equal(settings, want) {
		t.Fatalf("dropped settings = %v, want %v", settings, want)
	}
	if !slices.Equal(tenancy, []string{"Team/platform"}) {
		t.Fatalf("refused tenancy docs = %v, want [Team/platform]", tenancy)
	}
	if len(keep) != 1 || keep[0].Provider == nil {
		t.Fatalf("kept %d documents, want the Provider only", len(keep))
	}
}
