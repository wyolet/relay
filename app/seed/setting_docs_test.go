package seed

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

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
