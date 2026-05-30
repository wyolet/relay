package settings

import "fmt"

// SectionUsageLogging is the section key for the log (usage) event sink's
// backend selection. The log observer reconciles the live value and
// hot-swaps its sink + reader without a restart. Mirrors payload-logging,
// minus the per-request opt-in (logging is constant — every request).
//
// Connection-level config (DSNs) stays bootstrap-tier (boot env), the same
// way the PG DSN does: you need a connection before you can read settings.
// Only the backend selection + safe per-backend knobs live here.
const SectionUsageLogging = "usage-logging"

// UsageLogging selects and tunes the log/usage event backend. Per the config
// direction (minimize env), the backend choice lives here, not in env; the
// legacy RELAY_EVENTLOG_BACKEND is honored only as an interim fallback when
// this section is unset, until the YAML→DB settings seed lands.
type UsageLogging struct {
	// Backend selects the sink+reader: "file" (default), "clickhouse",
	// "postgres", or "valkey".
	Backend string `json:"backend"`

	File UsageFile       `json:"file"`
	CH   UsageClickHouse `json:"clickhouse"`
}

// UsageFile configures the JSONL file backend.
type UsageFile struct {
	Path string `json:"path,omitempty"`
}

// UsageClickHouse holds the safe-to-hot-swap CH knobs. The DSN reuses the
// boot CH connection (bootstrap-tier), so no credentials live in this row.
type UsageClickHouse struct {
	RetentionDays int    `json:"retentionDays,omitempty"`
	WALDir        string `json:"walDir,omitempty"`
}

// Validate enforces the backend enum + non-negative knobs.
func (u *UsageLogging) Validate() error {
	switch u.Backend {
	case "", "file", "clickhouse", "postgres", "valkey":
	default:
		return fmt.Errorf("usage-logging: backend must be \"file\", \"clickhouse\", \"postgres\", or \"valkey\", got %q", u.Backend)
	}
	if u.CH.RetentionDays < 0 {
		return fmt.Errorf("usage-logging: clickhouse.retentionDays must be >= 0")
	}
	return nil
}

func init() {
	Register(Section{
		Name:        SectionUsageLogging,
		Description: "Log (usage) event backend selection (file|clickhouse|postgres|valkey) + ClickHouse retention/WAL knobs. DSNs are bootstrap config (env). Hot-reloaded — backend reroute takes effect without a restart (reroute is a clean break, not a data migration).",
		// Empty backend = unset → the composition root falls back to the
		// legacy RELAY_EVENTLOG_BACKEND env (interim, until the YAML→DB seed
		// lands), then to "file". An explicit value here overrides env.
		Defaults: func() any {
			return &UsageLogging{}
		},
		Decode: decodeAndValidate[UsageLogging, *UsageLogging],
	})
}
