package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/metricslog"
	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settingswatch"
	"github.com/wyolet/relay/app/tokencount"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

func buildUsageLog(listenerCtx context.Context, cfg *config.Config, cat *appcatalog.Catalog, kvStore kv.Store,
	lifecycleReg *lifecycle.Registry) (*usagelog.Controller, usagelog.Reader) {
	// Log (usage) emit: the constant PostFlight observer (one event per
	// request). Backend selection lives in the "usage-logging" settings
	// section (hot-swappable, reroute = clean break); the legacy
	// RELAY_EVENTLOG_BACKEND is an interim fallback when the section is unset.
	// DSNs stay bootstrap-tier (env). The Controller hot-swaps both the sink
	// (emitter) and the reader (control plane) on a settings change.
	usagePath := os.Getenv("RELAY_USAGE_LOG")
	if usagePath == "" {
		usagePath = "relay-usage.jsonl"
	}
	usageWALDir := cfg.EventlogDir
	if usageWALDir == "" {
		usageWALDir = "relay-usage-wal"
	}
	usageCtl := usagelog.NewController(cat, usageBackendBuilder(usageBackendBoot{
		EnvBackend: cfg.EventlogBackend,
		CHDSN:      cfg.CHDSN,
		PGDSN:      cfg.PGDSN,
		KV:         kvStore,
		FilePath:   usagePath,
		WALDir:     usageWALDir,
	}), slog.Default())
	usageReader := usageCtl.Reader()
	// Emit-time cost: the usage producer prices each event's tokens against
	// the pricing the plan resolved (id stamped on the lifecycle Context),
	// read from the live snapshot — a map lookup, post-flight only.
	usagePricer := usagelog.NewPricer(func(id string) (*pricing.Pricing, bool) {
		return cat.Current().Pricing(id)
	})
	lifecycleReg.RegisterHook(usagelog.NewUsageHook(usagePricer, cfg.InstanceID))
	lifecycleReg.RegisterCollector(usagelog.NewSinkCollector(usageCtl.Emitter()))
	lifecycleReg.RegisterStreamObserver(usagelog.NewStreamUsageFactory(usagePricer, cfg.InstanceID))
	usageCtl.Subscribe() // synchronous: register before Hydrate so the boot reload reaches it
	go usageCtl.Run(listenerCtx)
	slog.Debug("usagelog: observer wired (backend via settings: usage-logging)")
	return usageCtl, usageReader
}

func payloadCHBootConfig(cfg *config.Config) payloadCHBoot {
	return payloadCHBoot{
		DSN:    cfg.CHDSN,
		WALDir: "relay-payload-wal",
	}
}

func buildPayloadLog(listenerCtx context.Context, cat *appcatalog.Catalog, stores *appcatalog.Stores, lifecycleReg *lifecycle.Registry,
	payloadCHBootCfg payloadCHBoot) *payloadlog.Controller {
	payloadCtl := payloadlog.NewController(cat, payloadSinkBuilder(stores.Secrets, payloadCHBootCfg), slog.Default())
	lifecycleReg.RegisterHook(payloadlog.NewPayloadHook(payloadCtl))
	lifecycleReg.RegisterCollector(payloadlog.NewSinkCollector(payloadCtl.Emitter()))
	lifecycleReg.RegisterStreamObserver(payloadlog.NewStreamPayloadFactory(payloadCtl))
	payloadCtl.Subscribe() // synchronous: register before Hydrate so the boot reload reaches it
	go payloadCtl.Run(listenerCtx)
	slog.Debug("payloadlog: observer wired (config via settings: payload-logging)")
	return payloadCtl
}

func buildTokenCalibrator(kvStore kv.Store, lifecycleReg *lifecycle.Registry) *tokencount.Calibrator {
	// Token-count calibration: every completed request teaches relay the bytes-to-tokens ratio of this session and this model, which is how the count-tokens endpoint answers for upstreams that expose no counter. A collector, so it reads the input-token count the usage producer already parsed; one kv write per completed request, post-flight only.
	tokenCalibrator := tokencount.NewCalibrator(kvStore)
	lifecycleReg.RegisterCollector(tokencount.NewObserver(tokenCalibrator))
	return tokenCalibrator
}

func registerAdmission(cfg *config.Config, lifecycleReg *lifecycle.Registry) {
	// Admission control: a per-pod in-flight cap on inference requests. Rides
	// the lifecycle spine — PreFlight (acquire) registered BEFORE the metrics
	// pre-flight so a shed request is never counted as in-flight, Collect
	// (release) fires from Finalize at response-body close so a streamed request
	// holds its slot for the whole stream. Scope is Dispatch only (inference +
	// each WS frame), never /healthz or the control plane. RELAY_MAX_INFLIGHT
	// tunes the cap; 0 = httpapi.DefaultMaxInflight.
	admission := httpapi.NewAdmission(cfg.MaxInflight)
	lifecycleReg.RegisterPreFlight(admission.PreFlight)
	lifecycleReg.RegisterCollector(admission)
	slog.Debug("admission: in-flight cap wired", "max_inflight", admission.Cap())
}

func registerMetrics(lifecycleReg *lifecycle.Registry, usageCtl *usagelog.Controller, payloadCtl *payloadlog.Controller) {
	// Metrics: the Prometheus observer. Reads request outcome + timing in
	// post-flight and emits the request-flow metrics via pkg/metrics. Pure
	// boot wiring — no runner changes. The data-loss
	// and provider-key metrics emit at their sources (emitters, keypool).
	metricsObs := metricslog.New()
	lifecycleReg.RegisterPreFlight(metricsObs.PreFlight)
	lifecycleReg.RegisterHook(metricsObs)
	lifecycleReg.RegisterStreamObserver(metricsObs) // streamed requests skip Fill; emit here
	lifecycleReg.RegisterCollector(metricsObs)
	// post_flight_seconds is emitted by the runners themselves (whole detached
	// goroutine incl. commit RTTs) — no finalize observer needed.
	metrics.RegisterQueueDepth("usage", func() float64 { return float64(usageCtl.Emitter().QueueDepth()) })
	metrics.RegisterQueueDepth("payload", func() float64 { return float64(payloadCtl.Emitter().QueueDepth()) })
	slog.Debug("metricslog: observer wired (/metrics on control plane)")
}

func buildAudit(st *storagemod.Storage, cat *appcatalog.Catalog) (*audit.Store, *audit.Emitter) {
	// Admin audit log: bounded emitter → PG, retention from the "audit"
	// settings section. Wired before hydration so the first settings reload
	// lands on the emitter.
	auditStore := audit.NewStore(gen.New(st.Pool()))
	auditEmitter := audit.NewEmitter(auditStore, slog.Default())
	settingswatch.New(cat, settings.SectionAudit, func(a settings.Audit) {
		auditEmitter.SetRetentionDays(a.RetentionDays)
	}, slog.Default()).Start()
	return auditStore, auditEmitter
}
