//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/seed"
	storagemod "github.com/wyolet/relay/internal/storage"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// The boot seed validates every row, so catalog data shipped on purpose must
// pass the same checks: a fresh install that cannot seed has no catalog at
// all. The embedded catalog is the shipped data the repo carries, rebuilt
// here as manifest documents and seeded through the boot path.
func TestIntegration_EmbeddedCatalogSeedsCleanly(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)
	truncateAll(t, st)

	idx, err := sdkcatalog.Load()
	if err != nil {
		t.Fatalf("load embedded catalog: %v", err)
	}
	dir := t.TempDir()
	want := writeCatalogManifests(t, idx.Catalog, filepath.Join(dir, "catalog.yaml"))

	res, err := seed.Run(ctx, seed.Options{Pool: st.Pool(), YAMLDir: dir, CatalogKindsOnly: true})
	if err != nil {
		t.Fatalf("seed embedded catalog: %v", err)
	}
	got := map[string]int{
		"Provider": res.Providers, "Host": res.Hosts, "Model": res.Models,
		"HostBinding": res.HostBindings, "Pricing": res.Pricings,
	}
	for kind, n := range want {
		if got[kind] != n {
			t.Errorf("%s: seeded %d, want %d", kind, got[kind], n)
		}
	}

	// The snapshot build must keep every seeded row, not drop it as invalid.
	cat, _, _, err := appcatalog.Bootstrap(ctx, appcatalog.BootstrapOptions{Pool: st.Pool()})
	if err != nil {
		t.Fatalf("catalog.Bootstrap: %v", err)
	}
	snap := cat.Current()
	if n := len(snap.EnabledHosts()); n != want["Host"] {
		t.Errorf("snapshot hosts = %d, want %d", n, want["Host"])
	}
	if n := len(snap.EnabledModels()); n != want["Model"] {
		t.Errorf("snapshot models = %d, want %d", n, want["Model"])
	}
}

// writeCatalogManifests renders c as one multi-document manifest file — one
// Model per served snapshot slug, one HostBinding and Pricing per (slug, host)
// — and returns the document count per kind.
func writeCatalogManifests(t *testing.T, c *sdkcatalog.Catalog, path string) map[string]int {
	t.Helper()
	var buf bytes.Buffer
	counts := map[string]int{}
	add := func(kind string, doc any) {
		out, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal %s: %v", kind, err)
		}
		buf.WriteString("---\n")
		buf.Write(out)
		counts[kind]++
	}

	providers := map[string]bool{}
	for _, p := range c.Providers {
		providers[p.Name] = true
		add("Provider", manifest.ProviderDTO{
			APIVersion: manifest.APIVersion, Kind: "Provider",
			Metadata: manifest.WireMeta{Name: p.Name, DisplayName: p.DisplayName, Description: p.Description},
			Spec: manifest.ProviderSpec{
				HomepageURL: p.HomepageURL, DocsURL: p.DocsURL, StatusPageURL: p.StatusPageURL, Icon: iconAt(p.Icon),
			},
		})
	}
	infos := map[string]sdkcatalog.ModelInfo{}
	for _, mi := range c.Models {
		infos[mi.MetadataName] = mi
	}

	models := map[string]bool{}
	for _, h := range c.Hosts {
		add("Host", manifest.HostDTO{
			APIVersion: manifest.APIVersion, Kind: "Host",
			Metadata: manifest.WireMeta{Name: h.Name, DisplayName: h.DisplayName, Description: h.Description},
			Spec: manifest.HostSpec{
				BaseURL: h.BaseURL, HomepageURL: h.HomepageURL, DocsURL: h.DocsURL,
				ConsoleURL: h.ConsoleURL, StatusPageURL: h.StatusPageURL, Icon: iconAt(h.Icon),
			},
		})

		for _, b := range h.Models {
			if !models[b.MetadataName] {
				models[b.MetadataName] = true
				owner := ""
				if len(b.Providers) > 0 {
					owner = b.Providers[0]
				}
				if owner != "" && !providers[owner] {
					providers[owner] = true
					add("Provider", manifest.ProviderDTO{
						APIVersion: manifest.APIVersion, Kind: "Provider",
						Metadata: manifest.WireMeta{Name: owner},
					})
				}
				add("Model", modelDoc(t, b, infos[b.MetadataName], owner))
			}

			bd := manifest.HostBindingDTO{
				APIVersion: manifest.APIVersion, Kind: "HostBinding",
				Metadata: manifest.WireMeta{Name: fmt.Sprintf("binding-%d", counts["HostBinding"])},
				Spec:     manifest.HostBindingSpec{Model: b.MetadataName, Host: h.Name, Adapter: b.Adapter},
			}
			if len(b.Pricing) > 0 {
				pd := manifest.PricingDTO{
					APIVersion: manifest.APIVersion, Kind: "Pricing",
					Metadata: manifest.WireMeta{
						Name:  fmt.Sprintf("pricing-%d", counts["Pricing"]),
						Owner: manifest.WireOwner{Kind: meta.OwnerHost, Name: h.Name},
					},
					Spec: manifest.PricingSpec{Currency: "USD", TargetModels: []string{b.MetadataName}},
				}
				for _, r := range b.Pricing {
					pd.Spec.Rates = append(pd.Spec.Rates, manifest.PricingRateDTO{
						Meter: r.Meter, Unit: r.Unit, Amount: r.Amount, AboveTokens: r.AboveTokens,
					})
				}
				bd.Spec.Pricing = pd.Metadata.Name
				add("Pricing", pd)
			}
			add("HostBinding", bd)
		}
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write manifests: %v", err)
	}
	return counts
}

func modelDoc(t *testing.T, b sdkcatalog.Binding, mi sdkcatalog.ModelInfo, owner string) manifest.ModelDTO {
	t.Helper()
	// The SDK mirror carries the same json tags as the domain bag.
	raw, err := json.Marshal(mi.Capabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	var caps model.Capabilities
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatalf("unmarshal capabilities: %v", err)
	}
	snap := model.Snapshot{Name: b.MetadataName}
	if b.Name != b.MetadataName {
		snap.OriginalName = b.Name
	}
	return manifest.ModelDTO{
		APIVersion: manifest.APIVersion, Kind: "Model",
		Metadata: manifest.WireMeta{
			Name: b.MetadataName, DisplayName: mi.DisplayName, Description: mi.Description,
			Owner: manifest.WireOwner{Kind: meta.OwnerProvider, Name: owner},
		},
		Spec: manifest.ModelSpec{
			Family: mi.Family, Version: mi.Version, Capabilities: caps,
			Modalities:         model.Modalities{Input: mi.Modalities.Input, Output: mi.Modalities.Output},
			ContextWindowInput: mi.ContextWindowInput, ContextWindowOutput: mi.ContextWindowOutput,
			ContextWindowTotal: mi.ContextWindowTotal, MaxOutputTokens: mi.MaxOutputTokens,
			KnowledgeCutoff: mi.KnowledgeCutoff, ReleaseDate: mi.ReleaseDate, License: mi.License,
			Tags: mi.Tags, Documentation: mi.Documentation, ProviderModelPageURL: mi.ProviderModelPageURL,
			Snapshots: []model.Snapshot{snap}, Pointer: b.MetadataName, Aliases: b.Aliases,
		},
	}
}

func iconAt(path string) *meta.Icon {
	if path == "" {
		return nil
	}
	return &meta.Icon{Path: path}
}
