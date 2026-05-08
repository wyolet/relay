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

	"github.com/wyolet/relay/internal/auth"
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
	APIKeys    [][]byte // already parsed via auth.ParseKeys
	AdminToken string
	MasterKey  []byte // already parsed via crypto.ParseMasterKey; nil if unset

	// Behavior knobs
	RichParsing       bool
	AdminReloadRPM    int
	CHRetentionDays   int
	AutoSeedIfEmpty   bool
	ConfigDir         string
	InstanceID        string
	EventlogDir       string
	MaxRequestBytes   int64 // 0 = use httpmw.DefaultMaxRequestBytes
	HealthzDeadlineMS int
	ShutdownDeadlineS int

	// ControlPort is the listener port for the control-plane HTTP server.
	// Empty disables the control listener entirely (data plane only).
	ControlPort string
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

	// --- Connections ---
	cfg.PGDSN = os.Getenv("RELAY_PG_DSN")
	cfg.RedisAddr = os.Getenv("RELAY_REDIS_ADDR")
	cfg.CHDSN = os.Getenv("RELAY_CH_DSN")
	cfg.OTLPEndpoint = os.Getenv("RELAY_OTLP_ENDPOINT")

	// --- Auth ---
	cfg.APIKeys = auth.ParseKeys(os.Getenv("RELAY_API_KEY"), os.Getenv("RELAY_API_KEYS"))
	cfg.AdminToken = os.Getenv("RELAY_ADMIN_TOKEN")

	// --- Behavior knobs ---
	cfg.AdminReloadRPM = envInt("RELAY_ADMIN_RELOAD_RPM", 10)
	cfg.CHRetentionDays = envInt("RELAY_CH_RETENTION_DAYS", 90)
	cfg.AutoSeedIfEmpty = os.Getenv("RELAY_AUTO_SEED_IF_EMPTY") == "1"

	cfg.ConfigDir = os.Getenv("RELAY_CONFIG_DIR")
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = "config"
	}

	cfg.InstanceID = os.Getenv("RELAY_INSTANCE_ID")
	cfg.EventlogDir = os.Getenv("RELAY_EVENTLOG_DIR")

	if v := envInt64("RELAY_MAX_REQUEST_BYTES", 0); v > 0 {
		cfg.MaxRequestBytes = v
	}

	cfg.HealthzDeadlineMS = envInt("RELAY_HEALTHZ_DEADLINE_MS", 500)
	cfg.ShutdownDeadlineS = envInt("RELAY_SHUTDOWN_DEADLINE_S", 15)

	cfg.ControlPort = os.Getenv("RELAY_CONTROL_PORT")
	if cfg.ControlPort == "" {
		cfg.ControlPort = "8081"
	}

	return cfg, nil
}
