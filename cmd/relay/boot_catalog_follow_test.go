package main

import (
	"testing"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/internal/config"
)

// Only a relay on latest follows the catalog, and only one with a webhook base
// registers for release webhooks.
func TestCatalogFollowWiring(t *testing.T) {
	var opts appcatalog.BootstrapOptions
	pinned := &config.Config{CatalogVersion: "v0.2.1", CatalogWebhookURL: "https://relay.example.com"}
	if f := buildCatalogFollower(pinned, nil, opts); f != nil {
		t.Fatal("an exact pin must not run a follower")
	}
	if catalogSync(nil) != nil {
		t.Fatal("a relay without a follower must leave the sync endpoint unwired")
	}

	latest := &config.Config{CatalogVersion: "latest"}
	f := buildCatalogFollower(latest, nil, opts)
	if f == nil {
		t.Fatal("latest must run a follower")
	}
	if reg := buildCatalogRegistrar(latest, f); reg != nil {
		t.Fatal("no RELAY_CATALOG_WEBHOOK_URL must register nothing")
	}
	latest.CatalogWebhookURL = "https://relay.example.com"
	reg := buildCatalogRegistrar(latest, f)
	if reg == nil || reg.HookURL != "https://relay.example.com/catalog/hooks/release" {
		t.Fatalf("registrar = %+v", reg)
	}
	if buildCatalogFollower(&config.Config{CatalogVersion: "auto"}, nil, opts) == nil {
		t.Fatal("auto must run a follower")
	}
}
