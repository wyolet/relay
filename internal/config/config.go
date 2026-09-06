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
	"log/slog"
	"os"
	"strings"

	"github.com/wyolet/relay/pkg/crypto"
)

// Authorizer modes for RELAY_AUTHZ.
const (
	AuthzSingle = "single"
	AuthzRBAC   = "rbac"
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
	PGDSN      string
	PGMaxConns int // RELAY_PG_MAX_CONNS; 0 = storage default (10)
	PGMinConns int // RELAY_PG_MIN_CONNS warm floor; 0 = storage default (2)
	RedisAddr  string
	// RedisPoolSize / RedisMinIdleConns tune the go-redis client pool.
	// 0 = library defaults (pool 10×GOMAXPROCS, NO idle floor). A warm
	// floor keeps connections pre-dialed so request bursts never pay
	// cold-dial (TCP + uncached DNS) latency inside a request budget —
	// same rationale as the PG warm floor above.
	RedisPoolSize     int // RELAY_REDIS_POOL_SIZE
	RedisMinIdleConns int // RELAY_REDIS_MIN_IDLE_CONNS
	CHDSN             string
	OTLPEndpoint      string

	// PublicURL is the deployment's externally reachable control-plane
	// origin (RELAY_PUBLIC_URL). Exported manifests reference the schema
	// endpoint under it; empty renders the reference relative.
	PublicURL string

	// Auth
	AdminToken string
	MasterKey  []byte // already parsed via crypto.ParseMasterKey; nil if unset

	// Authz selects the control-plane authorizer. "single" (the default)
	// grants every authenticated caller every action; "rbac" evaluates the
	// caller's RoleBindings against each row's scope chain.
	Authz string

	// MigrateOnBoot runs the pending up-migrations at startup
	// (RELAY_MIGRATE_ON_BOOT, default true). Set it false while rolling the
	// schema back, so a restarting pod doesn't re-apply what was just undone.
	MigrateOnBoot bool

	// Behavior knobs
	CHRetentionDays int
	AutoSeedIfEmpty bool
	ConfigDir       string
	CatalogDir      string
	CatalogVersion  string
	CatalogURL      string
	CatalogIndexURL string
	InstanceID      string
	EventlogDir     string
	MaxRequestBytes int64 // 0 = use httpmw.DefaultMaxRequestBytes
	MaxInflight     int   // RELAY_MAX_INFLIGHT per-pod in-flight cap; 0 = httpapi.DefaultMaxInflight

	// UpstreamMaxIdlePerHost caps idle keep-alive connections the upstream
	// transport pools per host (RELAY_UPSTREAM_MAX_IDLE_PER_HOST, default 128).
	// The stdlib default (2) forces a fresh dial on nearly every request at
	// high per-host RPS; a higher ceiling keeps hot upstream connections warm.
	UpstreamMaxIdlePerHost int

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
	// Pool sizing (0 = storage default). A higher MinConns keeps connections
	// pre-warmed so bursty control-plane load never pays cold-connection dial +
	// DNS latency on the request path.
	if v, err := envPositiveInt("RELAY_PG_MAX_CONNS", 0); err != nil {
		return nil, fmt.Errorf("RELAY_PG_MAX_CONNS must be >= 1")
	} else {
		cfg.PGMaxConns = v
	}
	if v, err := envPositiveInt("RELAY_PG_MIN_CONNS", 0); err != nil {
		return nil, fmt.Errorf("RELAY_PG_MIN_CONNS must be >= 1")
	} else {
		cfg.PGMinConns = v
	}
	cfg.RedisAddr = os.Getenv("RELAY_REDIS_ADDR")
	if v, err := envPositiveInt("RELAY_REDIS_POOL_SIZE", 0); err != nil {
		return nil, fmt.Errorf("RELAY_REDIS_POOL_SIZE must be >= 1")
	} else {
		cfg.RedisPoolSize = v
	}
	if v, err := envPositiveInt("RELAY_REDIS_MIN_IDLE_CONNS", 0); err != nil {
		return nil, fmt.Errorf("RELAY_REDIS_MIN_IDLE_CONNS must be >= 1")
	} else {
		cfg.RedisMinIdleConns = v
	}
	cfg.CHDSN = os.Getenv("RELAY_CH_DSN")
	cfg.OTLPEndpoint = os.Getenv("RELAY_OTLP_ENDPOINT")

	// --- Auth ---
	cfg.AdminToken = os.Getenv("RELAY_ADMIN_TOKEN")
	cfg.PublicURL = strings.TrimSuffix(os.Getenv("RELAY_PUBLIC_URL"), "/")

	// --- RELAY_AUTHZ ---
	switch v := os.Getenv("RELAY_AUTHZ"); v {
	case "":
		// RELAY_MULTI_USER is what pre-IAM deployments set. Ignoring it
		// would silently drop an upgraded multi-user relay to "single",
		// where every authenticated user is an admin.
		if multiUserOn() {
			slog.Warn("config: RELAY_MULTI_USER is deprecated and was read as RELAY_AUTHZ=rbac; set RELAY_AUTHZ explicitly")
			cfg.Authz = AuthzRBAC
		} else {
			cfg.Authz = AuthzSingle
		}
	case AuthzSingle:
		// An explicit "single" wins, but the pair is contradictory: the
		// deployment asked for multi-user and gets every caller an admin.
		if multiUserOn() {
			slog.Warn("config: RELAY_AUTHZ=single with RELAY_MULTI_USER=on — every authenticated caller is an admin; set RELAY_AUTHZ=rbac for role-based access")
		}
		cfg.Authz = AuthzSingle
	case AuthzRBAC:
		cfg.Authz = AuthzRBAC
	default:
		return nil, fmt.Errorf(`RELAY_AUTHZ must be %q or %q, got %q`, AuthzSingle, AuthzRBAC, v)
	}

	// --- RELAY_MIGRATE_ON_BOOT ---
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_MIGRATE_ON_BOOT"))); v {
	case "", "on", "1", "true":
		cfg.MigrateOnBoot = true
	case "off", "0", "false":
		cfg.MigrateOnBoot = false
	default:
		return nil, fmt.Errorf(`RELAY_MIGRATE_ON_BOOT must be "on" or "off", got %q`, v)
	}

	// --- Behavior knobs ---
	if v, err := envPositiveInt("RELAY_CH_RETENTION_DAYS", 90); err != nil {
		return nil, fmt.Errorf("RELAY_CH_RETENTION_DAYS must be >= 1")
	} else {
		cfg.CHRetentionDays = v
	}
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
	// This is the dev/airgapped escape hatch; CatalogVersion below is the
	// production path.
	cfg.CatalogDir = os.Getenv("RELAY_CATALOG_DIR")

	// CatalogVersion pins the seeded catalog to a published relay-catalog
	// ref (e.g. "v0.1.0"), or tracks the newest release for this binary's
	// schema channel as "latest"/"auto" (resolved via CatalogIndexURL at
	// every boot). At boot the stored catalog-source marker is compared
	// against the concrete version; on mismatch the tree is seeded — from
	// the local CatalogDir when its .version stamp matches (baked image
	// or self-stamped custom catalog, no network), else fetched from
	// CatalogURL (default the wyolet/relay-catalog GitHub archive) — no
	// image rebuild to move catalog versions. Re-seeds skip
	// operator-edited (dirty) rows and overlays re-merge at snapshot load,
	// so user changes survive. Unset = seed-if-empty from CatalogDir only.
	cfg.CatalogVersion = os.Getenv("RELAY_CATALOG_VERSION")
	cfg.CatalogURL = os.Getenv("RELAY_CATALOG_URL")
	cfg.CatalogIndexURL = os.Getenv("RELAY_CATALOG_INDEX_URL")

	cfg.InstanceID = os.Getenv("RELAY_INSTANCE_ID")
	cfg.EventlogDir = os.Getenv("RELAY_EVENTLOG_DIR")

	if v := envInt64("RELAY_MAX_REQUEST_BYTES", 0); v > 0 {
		cfg.MaxRequestBytes = v
	}

	// RELAY_MAX_INFLIGHT bounds concurrent in-flight inference requests per pod
	// (the admission cap). 0 = derived default (app/httpapi.DefaultMaxInflight),
	// applied where the Admission is constructed.
	cfg.MaxInflight = envInt("RELAY_MAX_INFLIGHT", 0)

	if v, err := envPositiveInt("RELAY_UPSTREAM_MAX_IDLE_PER_HOST", 128); err != nil {
		return nil, fmt.Errorf("RELAY_UPSTREAM_MAX_IDLE_PER_HOST must be >= 1")
	} else {
		cfg.UpstreamMaxIdlePerHost = v
	}

	if v, err := envPositiveInt("RELAY_HEALTHZ_DEADLINE_MS", 500); err != nil {
		return nil, fmt.Errorf("RELAY_HEALTHZ_DEADLINE_MS must be >= 1")
	} else {
		cfg.HealthzDeadlineMS = v
	}
	if v, err := envPositiveInt("RELAY_SHUTDOWN_DEADLINE_S", 15); err != nil {
		return nil, fmt.Errorf("RELAY_SHUTDOWN_DEADLINE_S must be >= 1")
	} else {
		cfg.ShutdownDeadlineS = v
	}

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

// multiUserOn reports whether the deprecated RELAY_MULTI_USER flag is set to
// something truthy. Anything else — including its old "off" — leaves the
// authorizer alone.
func multiUserOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_MULTI_USER"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}
