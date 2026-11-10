package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// loadCatalogFollow reads the settings of a relay following the latest
// catalog release: how often it checks, the boot downgrade opt-in, and the
// public base the catalog server sends release webhooks to.
func loadCatalogFollow(cfg *Config) error {
	interval, err := envDuration("RELAY_CATALOG_POLL_INTERVAL", time.Hour)
	if err != nil || interval < 0 {
		return fmt.Errorf("RELAY_CATALOG_POLL_INTERVAL must be a non-negative Go duration such as 1h or 15m (0 disables polling), got %q", os.Getenv("RELAY_CATALOG_POLL_INTERVAL"))
	}
	cfg.CatalogPollInterval = interval
	cfg.CatalogAllowDowngrade = os.Getenv("RELAY_CATALOG_ALLOW_DOWNGRADE") == "1"

	raw := os.Getenv("RELAY_CATALOG_WEBHOOK_URL")
	if raw == "" {
		return nil
	}
	base, err := webhookBase(raw)
	if err != nil {
		return fmt.Errorf("RELAY_CATALOG_WEBHOOK_URL: %w", err)
	}
	cfg.CatalogWebhookURL = base
	return nil
}

// webhookBase validates the public base URL of this relay's inference
// listener. Plain http is accepted only under the plain-HTTP opt-out the
// session cookie uses (RELAY_COOKIE_SECURE=false), for local setups.
func webhookBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	plainHTTP := u.Scheme == "http" && os.Getenv("RELAY_COOKIE_SECURE") == "false"
	if u.Host == "" || (u.Scheme != "https" && !plainHTTP) {
		return "", fmt.Errorf("must be an https URL such as https://relay.example.com, got %q (http needs RELAY_COOKIE_SECURE=false)", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("must be a base URL without credentials, query or fragment, got %q", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}
