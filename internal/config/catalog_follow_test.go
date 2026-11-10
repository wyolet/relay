package config

import (
	"strings"
	"testing"
	"time"
)

func TestCatalogFollowDefaults(t *testing.T) {
	t.Setenv("RELAY_CATALOG_POLL_INTERVAL", "")
	t.Setenv("RELAY_CATALOG_ALLOW_DOWNGRADE", "")
	t.Setenv("RELAY_CATALOG_WEBHOOK_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogPollInterval != time.Hour || cfg.CatalogAllowDowngrade || cfg.CatalogWebhookURL != "" {
		t.Fatalf("defaults = %v, %v, %q", cfg.CatalogPollInterval, cfg.CatalogAllowDowngrade, cfg.CatalogWebhookURL)
	}
}

func TestCatalogFollowSettings(t *testing.T) {
	t.Setenv("RELAY_CATALOG_POLL_INTERVAL", "0")
	t.Setenv("RELAY_CATALOG_ALLOW_DOWNGRADE", "1")
	t.Setenv("RELAY_CATALOG_WEBHOOK_URL", "https://relay.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogPollInterval != 0 || !cfg.CatalogAllowDowngrade || cfg.CatalogWebhookURL != "https://relay.example.com" {
		t.Fatalf("parsed = %v, %v, %q", cfg.CatalogPollInterval, cfg.CatalogAllowDowngrade, cfg.CatalogWebhookURL)
	}
}

func TestCatalogFollowRefusesBadValues(t *testing.T) {
	for _, tc := range []struct {
		name, env, value, cookieSecure, wantErr string
	}{
		{"not a duration", "RELAY_CATALOG_POLL_INTERVAL", "hourly", "", "RELAY_CATALOG_POLL_INTERVAL"},
		{"negative interval", "RELAY_CATALOG_POLL_INTERVAL", "-1m", "", "RELAY_CATALOG_POLL_INTERVAL"},
		{"plain http", "RELAY_CATALOG_WEBHOOK_URL", "http://relay.example.com", "", "https"},
		{"no host", "RELAY_CATALOG_WEBHOOK_URL", "relay.example.com", "", "https"},
		{"query", "RELAY_CATALOG_WEBHOOK_URL", "https://relay.example.com/?a=1", "", "query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_CATALOG_POLL_INTERVAL", "")
			t.Setenv("RELAY_CATALOG_WEBHOOK_URL", "")
			t.Setenv("RELAY_COOKIE_SECURE", tc.cookieSecure)
			t.Setenv(tc.env, tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// A plain-HTTP deployment (the session cookie's opt-out) may name an http base.
func TestCatalogWebhookURLPlainHTTPOptOut(t *testing.T) {
	t.Setenv("RELAY_COOKIE_SECURE", "false")
	t.Setenv("RELAY_CATALOG_WEBHOOK_URL", "http://relay.local:8080")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogWebhookURL != "http://relay.local:8080" {
		t.Fatalf("CatalogWebhookURL = %q", cfg.CatalogWebhookURL)
	}
}
