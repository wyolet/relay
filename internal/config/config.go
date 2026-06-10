// Package config centralizes all RELAY_* env var parsing for the relay binary.
// It is the single source of truth for the env contract; grep here to learn
// what env vars Relay reads.
//
// Load() validates inputs at boot (master key shape, cluster-mode enum, etc.)
// so subsystem constructors can trust the fields they receive. Subsystems do
// NOT read env vars themselves — they accept values via their own typed configs.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/wyolet/relay/pkg/crypto"
)

// Config holds every RELAY_* setting parsed and validated at startup.
type Config struct {
	// Cluster
	ClusterMode bool

	// Backends
	CatalogBackend  string
	StateBackend    string
	EventlogBackend string

	// Connections
	PGDSN        string
	RedisAddr    string
	CHDSN        string
	OTLPEndpoint string

	// Auth
	AdminToken string
	MasterKey  []byte // already parsed via crypto.ParseMasterKey; nil if unset

	// Behavior knobs
	CHRetentionDays int
	AutoSeedIfEmpty bool
	ConfigDir       string
	CatalogDir      string
	InstanceID      string
	EventlogDir     string
	MaxRequestBytes int64 // 0 = use httpmw.DefaultMaxRequestBytes

	// Payload logging has no env knobs — its config (enable, backend, S3
	// settings, credentials) lives in the runtime "payload-logging" settings
	// section and hot-reloads without a restart. See app/settings +
	// app/payloadlog.Controller.

	HealthzDeadlineMS int
	ShutdownDeadlineS int

	// DevTrustEventTime makes the inference edge honor the X-WR-Event-Time
	// header as the usage Event timestamp (RELAY_DEV_TRUST_EVENT_TIME=1).
	// Dev/replay tooling only — never enable in production.
	DevTrustEventTime bool

	// ControlPort is the listener port for the control-plane HTTP server.
	// Empty disables the control listener entirely (data plane only).
	ControlPort string

	// ControlAllowOrigins is the CORS allowlist for the control API. Comma-
	// separated list of exact origin strings (no wildcards — credentialed
	// CORS forbids them). Empty disables CORS entirely.
	ControlAllowOrigins []string

	// UIDisable suppresses serving the embedded admin UI on the control
	// listener (RELAY_UI_DISABLE=1). Off by default; the UI is same-origin
	// and adds no surface beyond the control API it already fronts.
	UIDisable bool

	// Runtime is the public, unauthenticated runtime config the embedded admin
	// UI fetches at boot via GET /config.json. Public values only — it is
	// world-readable. All fields are optional; empty URL fields make the UI
	// fall back to its own origin (which is correct for controlApiUrl in the
	// single-binary case, but NOT for inferenceApiUrl when the data plane is a
	// separate origin — hence RELAY_INFERENCE_API_URL).
	Runtime RuntimeConfig
}

// RuntimeConfig carries the deployment-specific values surfaced to the browser
// via GET /config.json. Mirrors the JSON the UI reads; see config_json.go.
type RuntimeConfig struct {
	ControlAPIURL   string // RELAY_CONTROL_API_URL   (empty ⇒ UI uses its origin)
	InferenceAPIURL string // RELAY_INFERENCE_API_URL (empty ⇒ UI prompts; no safe origin default)
	Mode            string // RELAY_MODE ("oss" | "cloud"); default "oss"
	SentryDSN       string // RELAY_UI_SENTRY_DSN        (public client-side DSN)
	TelemetryEnv    string // RELAY_UI_TELEMETRY_ENV
	DocsURL         string // RELAY_UI_DOCS_URL
	SupportURL      string // RELAY_UI_SUPPORT_URL
}

// Load reads every RELAY_* environment variable, validates them, and returns
// a fully-populated *Config. Returns a descriptive error on the first
// validation failure.
func Load() (*Config, error) {
	cfg := &Config{}

	// --- RELAY_CLUSTER_MODE ---
	switch v := os.Getenv("RELAY_CLUSTER_MODE"); v {
	case "", "off":
		cfg.ClusterMode = false
	case "on":
		cfg.ClusterMode = true
	default:
		return nil, fmt.Errorf(`RELAY_CLUSTER_MODE must be "on" or "off", got %q`, v)
	}

	// --- RELAY_MASTER_KEY ---
	if raw := os.Getenv("RELAY_MASTER_KEY"); raw != "" {
		mk, err := crypto.ParseMasterKey(raw)
		if err != nil {
			return nil, fmt.Errorf("RELAY_MASTER_KEY: %w", err)
		}
		cfg.MasterKey = mk
	}

	// --- Backends ---
	cfg.CatalogBackend = os.Getenv("RELAY_CATALOG_BACKEND")
	if cfg.CatalogBackend == "" {
		cfg.CatalogBackend = "yaml"
	}
	cfg.StateBackend = os.Getenv("RELAY_STATE_BACKEND")
	if cfg.StateBackend == "" {
		cfg.StateBackend = "memory"
	}
	cfg.EventlogBackend = os.Getenv("RELAY_EVENTLOG_BACKEND")
	if cfg.EventlogBackend == "" {
		cfg.EventlogBackend = "file"
	}
	switch cfg.EventlogBackend {
	case "file", "clickhouse", "valkey", "postgres":
	default:
		return nil, fmt.Errorf(`RELAY_EVENTLOG_BACKEND must be "file", "clickhouse", "valkey", or "postgres", got %q`, cfg.EventlogBackend)
	}

	// --- Connections ---
	cfg.PGDSN = os.Getenv("RELAY_PG_DSN")
	cfg.RedisAddr = os.Getenv("RELAY_REDIS_ADDR")
	cfg.CHDSN = os.Getenv("RELAY_CH_DSN")
	cfg.OTLPEndpoint = os.Getenv("RELAY_OTLP_ENDPOINT")

	// --- Auth ---
	cfg.AdminToken = os.Getenv("RELAY_ADMIN_TOKEN")

	// --- Behavior knobs ---
	cfg.CHRetentionDays = envInt("RELAY_CH_RETENTION_DAYS", 90)
	cfg.AutoSeedIfEmpty = os.Getenv("RELAY_AUTO_SEED_IF_EMPTY") == "1"

	cfg.ConfigDir = os.Getenv("RELAY_CONFIG_DIR")
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = "config"
	}

	// CatalogDir points at a local clone of wyolet/relay-catalog's data/
	// directory (or a forked equivalent). Used by Bootstrap auto-seed when
	// PG is empty. Empty disables auto-seed even if RELAY_AUTO_SEED_IF_EMPTY
	// is set — operators in airgapped/managed deploys leave it unset and
	// rely on admin API writes or a pre-populated DB.
	//
	// This is the dev/airgapped escape hatch; the tarball-fetcher (planned)
	// will be the production default once shipped.
	cfg.CatalogDir = os.Getenv("RELAY_CATALOG_DIR")

	cfg.InstanceID = os.Getenv("RELAY_INSTANCE_ID")
	cfg.EventlogDir = os.Getenv("RELAY_EVENTLOG_DIR")

	if v := envInt64("RELAY_MAX_REQUEST_BYTES", 0); v > 0 {
		cfg.MaxRequestBytes = v
	}

	cfg.HealthzDeadlineMS = envInt("RELAY_HEALTHZ_DEADLINE_MS", 500)
	cfg.ShutdownDeadlineS = envInt("RELAY_SHUTDOWN_DEADLINE_S", 15)

	cfg.DevTrustEventTime = os.Getenv("RELAY_DEV_TRUST_EVENT_TIME") == "1"

	cfg.UIDisable = os.Getenv("RELAY_UI_DISABLE") == "1"

	// Public UI runtime config (GET /config.json). URLs are trimmed of any
	// trailing slash so the UI can append paths cleanly.
	trimURL := func(k string) string { return strings.TrimRight(os.Getenv(k), "/") }
	cfg.Runtime = RuntimeConfig{
		ControlAPIURL:   trimURL("RELAY_CONTROL_API_URL"),
		InferenceAPIURL: trimURL("RELAY_INFERENCE_API_URL"),
		Mode:            os.Getenv("RELAY_MODE"),
		SentryDSN:       os.Getenv("RELAY_UI_SENTRY_DSN"),
		TelemetryEnv:    os.Getenv("RELAY_UI_TELEMETRY_ENV"),
		DocsURL:         trimURL("RELAY_UI_DOCS_URL"),
		SupportURL:      trimURL("RELAY_UI_SUPPORT_URL"),
	}
	if cfg.Runtime.Mode == "" {
		cfg.Runtime.Mode = "oss"
	}

	cfg.ControlPort = os.Getenv("RELAY_CONTROL_PORT")
	if cfg.ControlPort == "" {
		cfg.ControlPort = "8081"
	}

	if raw := os.Getenv("RELAY_CONTROL_ALLOW_ORIGINS"); raw != "" {
		for _, o := range strings.Split(raw, ",") {
			if t := strings.TrimSpace(o); t != "" {
				cfg.ControlAllowOrigins = append(cfg.ControlAllowOrigins, t)
			}
		}
	}

	return cfg, nil
}
