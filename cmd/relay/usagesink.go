package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/kv"
	chsink "github.com/wyolet/relay/pkg/usage/clickhouse"
	"github.com/wyolet/relay/pkg/usage/file"
	pgsink "github.com/wyolet/relay/pkg/usage/postgres"
	vksink "github.com/wyolet/relay/pkg/usage/valkey"
)

// usageBackendBoot carries the bootstrap-tier inputs the log backend builder
// needs — connection strings + the shared kv handle. Per the config
// direction (minimize env), only the backend *selection* lives in settings;
// DSNs stay bootstrap config (you need a connection before you can read
// settings). EnvBackend is the legacy RELAY_EVENTLOG_BACKEND, honored only
// as an interim fallback when the settings section is unset.
type usageBackendBoot struct {
	EnvBackend      string // legacy RELAY_EVENTLOG_BACKEND (interim fallback)
	CHDSN           string
	PGDSN           string
	KV              kv.Store
	FilePath        string
	WALDir          string
	CHRetentionDays int
}

// usageBackendBuilder returns the usagelog.BackendBuilder the Controller
// calls on each reconcile. The composition root is the only place log
// backend packages are named. Each backend's sink also serves as its reader
// (file is the exception — separate sink + reader).
func usageBackendBuilder(boot usageBackendBoot) usagelog.BackendBuilder {
	return func(ctx context.Context, cfg settings.UsageLogging) (usagelog.Backend, error) {
		backend := cfg.Backend
		if backend == "" {
			backend = boot.EnvBackend // interim: legacy env when section unset
		}
		switch backend {
		case "clickhouse":
			if boot.CHDSN == "" {
				return usagelog.Backend{}, fmt.Errorf("usagelog: clickhouse backend requires a CH DSN (RELAY_CH_DSN)")
			}
			retention := boot.CHRetentionDays
			if cfg.CH.RetentionDays > 0 {
				retention = cfg.CH.RetentionDays
			}
			walDir := boot.WALDir
			if cfg.CH.WALDir != "" {
				walDir = cfg.CH.WALDir
			}
			ch, err := chsink.New(chsink.Config{DSN: boot.CHDSN, RetentionDays: retention, WALDir: walDir})
			if err != nil {
				return usagelog.Backend{}, err
			}
			return usagelog.Backend{Sink: ch, Reader: ch}, nil
		case "postgres":
			pg, err := pgsink.New(ctx, pgsink.Config{DSN: boot.PGDSN})
			if err != nil {
				return usagelog.Backend{}, err
			}
			return usagelog.Backend{Sink: pg, Reader: pg}, nil
		case "valkey":
			vk := vksink.New(boot.KV, vksink.Config{})
			return usagelog.Backend{Sink: vk, Reader: vk}, nil
		default: // "file"
			path := cfg.File.Path
			if path == "" {
				path = boot.FilePath
			}
			fs, err := file.NewSink(path)
			if err != nil {
				// Match the legacy graceful fallback: log to stdout, read the
				// (possibly empty) file. Better than dropping silently at boot.
				slog.Warn("usagelog: file sink failed; using stdout", "err", err, "path", path)
				return usagelog.Backend{Sink: file.Stdout(), Reader: file.NewReader(path)}, nil
			}
			return usagelog.Backend{Sink: fs, Reader: file.NewReader(path)}, nil
		}
	}
}
