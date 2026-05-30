package main

import (
	"github.com/wyolet/relay/app/settings"
	chpayload "github.com/wyolet/relay/pkg/payload/clickhouse"
)

// payloadCHBoot carries the boot-time ClickHouse parameters for the payload
// backend. The DSN reuses the relay's CH connection (RELAY_CH_DSN — the same
// cluster the usage sink uses), so no credentials live in the hot-swappable
// settings row; only safe per-backend knobs (retention/WAL) come from there.
type payloadCHBoot struct {
	DSN           string
	RetentionDays int    // boot default; overridable per settings
	WALDir        string // boot default; overridable per settings
}

// config merges the boot defaults with the per-settings overrides into a
// concrete chpayload.Config. Zero overrides fall back to the boot value (and
// then the package defaults inside chpayload).
func (b payloadCHBoot) config(s settings.PayloadClickHouse) chpayload.Config {
	cfg := chpayload.Config{
		DSN:           b.DSN,
		RetentionDays: b.RetentionDays,
		WALDir:        b.WALDir,
	}
	if s.RetentionDays > 0 {
		cfg.RetentionDays = s.RetentionDays
	}
	if s.WALDir != "" {
		cfg.WALDir = s.WALDir
	}
	return cfg
}
