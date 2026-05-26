// Package config centralizes all RELAY_* env var parsing for the relay binary.
// It is the single source of truth for the env contract; grep here to learn
// what env vars Relay reads.
//
// Load() validates inputs at boot (master key shape, RICH_PARSING enum, etc.)
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
	RichParsing     bool
	AdminReloadRPM  int
	CHRetentionDays int
	AutoSeedIfEmpty bool
	ConfigDir       string
	CatalogDir      string
	InstanceID      string
	EventlogDir     string
	MaxRequestBytes int64 // 0 = use httpmw.DefaultMaxRequestBytes

	// PayloadLog enables the request/response body capture observer.
	// Off by default; per-request opt-in is via Policy/RelayKey.
	PayloadLog         bool
	PayloadLogBackend  string // "file" or "s3". Default "file".
	PayloadLogPath     string // file backend path. Default "relay-payloads.jsonl".
	PayloadLogMaxBytes int    // per-body storage cap; 0 = unlimited. Default 1 MiB.

	// S3 backend settings (used when PayloadLogBackend == "s3"). The s3
	// sink is excluded from the "minimal" build; selecting it there errors
	// at boot.
	PayloadLogS3Endpoint  string
	PayloadLogS3Bucket    string
	PayloadLogS3Region    string
	PayloadLogS3AccessKey string
	PayloadLogS3SecretKey string
	PayloadLogS3Prefix    string
	PayloadLogS3UseSSL    bool

	HealthzDeadlineMS int
	ShutdownDeadlineS int

	// ControlPort is the listener port for the control-plane HTTP server.
	// Empty disables the control listener entirely (data plane only).
	ControlPort string

	// ControlAllowOrigins is the CORS allowlist for the control API. Comma-
	// separated list of exact origin strings (no wildcards — credentialed
	// CORS forbids them). Empty disables CORS entirely.
	ControlAllowOrigins []string
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

	// --- RELAY_RICH_PARSING ---
	switch v := os.Getenv("RELAY_RICH_PARSING"); v {
	case "", "on":
		cfg.RichParsing = true
	case "off":
		cfg.RichParsing = false
	default:
		return nil, fmt.Errorf(`RELAY_RICH_PARSING must be "on" or "off", got %q`, v)
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
	cfg.AdminReloadRPM = envInt("RELAY_ADMIN_RELOAD_RPM", 10)
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

	cfg.PayloadLog = os.Getenv("RELAY_PAYLOADLOG") == "on" || os.Getenv("RELAY_PAYLOADLOG") == "1"
	cfg.PayloadLogBackend = os.Getenv("RELAY_PAYLOADLOG_BACKEND")
	if cfg.PayloadLogBackend == "" {
		cfg.PayloadLogBackend = "file"
	}
	switch cfg.PayloadLogBackend {
	case "file", "s3":
	default:
		return nil, fmt.Errorf(`RELAY_PAYLOADLOG_BACKEND must be "file" or "s3", got %q`, cfg.PayloadLogBackend)
	}
	cfg.PayloadLogPath = os.Getenv("RELAY_PAYLOADLOG_PATH")
	if cfg.PayloadLogPath == "" {
		cfg.PayloadLogPath = "relay-payloads.jsonl"
	}
	cfg.PayloadLogMaxBytes = envInt("RELAY_PAYLOADLOG_MAX_BYTES", 1<<20)

	cfg.PayloadLogS3Endpoint = os.Getenv("RELAY_PAYLOADLOG_S3_ENDPOINT")
	cfg.PayloadLogS3Bucket = os.Getenv("RELAY_PAYLOADLOG_S3_BUCKET")
	cfg.PayloadLogS3Region = os.Getenv("RELAY_PAYLOADLOG_S3_REGION")
	cfg.PayloadLogS3AccessKey = os.Getenv("RELAY_PAYLOADLOG_S3_ACCESS_KEY")
	cfg.PayloadLogS3SecretKey = os.Getenv("RELAY_PAYLOADLOG_S3_SECRET_KEY")
	cfg.PayloadLogS3Prefix = os.Getenv("RELAY_PAYLOADLOG_S3_PREFIX")
	cfg.PayloadLogS3UseSSL = os.Getenv("RELAY_PAYLOADLOG_S3_USE_SSL") != "false"
	if cfg.PayloadLog && cfg.PayloadLogBackend == "s3" && cfg.PayloadLogS3Bucket == "" {
		return nil, fmt.Errorf("RELAY_PAYLOADLOG_S3_BUCKET required when RELAY_PAYLOADLOG_BACKEND=s3")
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
