package main

import (
	"github.com/wyolet/relay/app/settings"
	chpayload "github.com/wyolet/relay/pkg/payload/clickhouse"
)

// payloadCHBoot carries the boot-time ClickHouse parameters for the payload
// backend. The DSN reuses the relay's CH connection (RELAY_CH_DSN — the same
// cluster the usage sink uses), so no credentials live in the hot-swappable
// settings row; only safe knobs (retention/WAL/dedup) come from there.
type payloadCHBoot struct {
	DSN    string
	WALDir string // boot default; overridable per settings
}

// config merges the boot defaults with the settings section into a concrete
// chpayload.Config.
func (b payloadCHBoot) config(s settings.PayloadLogging) chpayload.Config {
	cfg := chpayload.Config{
		DSN:           b.DSN,
		RetentionDays: s.RetentionDays,
		WALDir:        b.WALDir,
		Dedup:         s.CH.Dedup,
	}
	if s.CH.WALDir != "" {
		cfg.WALDir = s.CH.WALDir
	}
	return cfg
}
