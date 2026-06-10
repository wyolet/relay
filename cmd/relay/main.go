// Command relay is the wyolet-relay data + control plane binary.
//
// New-arch entrypoint: boots app/catalog, mounts the two HTTP planes from
// app/httpapi (inference + control) on separate listeners. Legacy wiring
// against internal/catalog has been moved aside under _legacy/ and will be
// deleted as routes/handlers are ported over.
package main

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/batch"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/metricslog"
	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/routing"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settingswatch"
	"github.com/wyolet/relay/app/usagelog"
	relayweb "github.com/wyolet/relay/cmd/relay/web"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/identity"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/jobq"
	"github.com/wyolet/relay/jobq/payload"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/reqid"
	pkganthropic "github.com/wyolet/relay/sdk/adapters/anthropic"
	pkggemini "github.com/wyolet/relay/sdk/adapters/gemini"
	pkgopenai "github.com/wyolet/relay/sdk/adapters/openai"
	relayv1 "github.com/wyolet/relay/sdk/v1"
)

func main() {
	loadDotEnv(".env")
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			slog.Debug("relay: 'migrate' subcommand currently runs implicitly on boot")
			return
		case "seed":
			if err := runSeed(os.Args[2:]); err != nil {
				slog.Error("seed failed", "err", err)
				os.Exit(1)
			}
			return
		}
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config invalid", "err", err)
		os.Exit(1)
	}
	if cfg.PGDSN == "" {
		slog.Error("RELAY_PG_DSN required (new-arch boot is PG-only)")
		os.Exit(1)
	}

	bootCtx := context.Background()

	st, err := storagemod.Open(bootCtx, cfg.PGDSN)
	if err != nil {
		slog.Error("storage.Open failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	bootOpts := appcatalog.BootstrapOptions{
		Pool:      st.Pool(),
		MasterKey: cfg.MasterKey,
	}
	if cfg.AutoSeedIfEmpty && cfg.CatalogDir != "" {
		bootOpts.AutoSeedDir = cfg.CatalogDir
	}

	// Stores-first: wire the catalog stores synchronously so the control
	// plane can serve CRUD even if the data-plane snapshot bootstrap
	// fails or stalls. Hydrate (seed + first Reload + NOTIFY listener)
	// runs in the background with retry — inference middleware gates
	// on catalog.IsReady() and returns 503 until the snapshot is built.
	cat, stores, err := appcatalog.BootstrapStores(bootCtx, bootOpts)
	if err != nil {
		slog.Error("catalog stores init failed", "err", err)
		os.Exit(1)
	}

	// First-boot / airgapped settings seed: upsert any <section>.yaml from the
	// settings dir that has no DB row yet (seed-if-absent — never clobbers a
	// runtime change). Managed deployments configure at runtime via the
	// settings API instead; this just bootstraps a fresh instance. Runs before
	// hydrate so the seeded values land in the snapshot's first reload.
	settingsDir := os.Getenv("RELAY_SETTINGS_DIR")
	if settingsDir == "" {
		settingsDir = filepath.Join(cfg.ConfigDir, "settings")
	}
	if seeded, err := settings.SeedDir(bootCtx, stores.Settings, settingsDir); err != nil {
		slog.Error("settings seed failed", "err", err, "dir", settingsDir)
		os.Exit(1)
	} else if len(seeded) > 0 {
		slog.Info("settings: seeded from YAML", "dir", settingsDir, "sections", seeded)
	}

	listenerCtx, cancelListener := context.WithCancel(bootCtx)
	defer cancelListener()
	// hydrateLoop launches below, after settings-change subscribers are
	// registered — its first Hydrate runs settings.reload, which notifies
	// subscribers with the stored values. Registering after it would race
	// that one-shot boot notification.

	// Identity store — fatal if YAML is malformed (login would silently
	// be disabled otherwise). Empty store is fine (login returns 503).
	idStore, err := identity.LoadYAML(cfg.ConfigDir)
	if err != nil {
		slog.Error("identity: load YAML failed", "err", err)
		os.Exit(1)
	}
	if n := len(idStore.Users()); n > 0 {
		slog.Debug("identity: loaded users", "count", n)
	}

	// kv backend — sessions, rate-limits, key-pool all share this.
	var kvStore kv.Store
	if cfg.StateBackend == "redis" {
		if cfg.RedisAddr == "" {
			slog.Error("RELAY_REDIS_ADDR required when RELAY_STATE_BACKEND=redis")
			os.Exit(1)
		}
		rs, err := kv.NewRedis(bootCtx, kv.RedisConfig{Addr: cfg.RedisAddr})
		if err != nil {
			slog.Error("state(redis) init failed", "err", err)
			os.Exit(1)
		}
		kvStore = rs
	} else {
		kvStore = kv.NewMem()
	}
	defer kvStore.Close()

	cookieSecure := os.Getenv("RELAY_COOKIE_SECURE") != "false"
	sessMgr := session.New(kvStore, cookieSecure, "sess:")

	// Pipeline orchestrator: shared limiter + selector backed by kv.
	limiter := pkgratelimit.New(kvStore, slog.Default(), nil)
	selector := keypool.New(kvStore, slog.Default(), nil, nil)
	hostHealth := hosthealth.New(kvStore, nil)
	policySvc := policy.NewService(catalogSnapReader{cat: cat}, selector, limiter)

	// Lifecycle registry — the single point where observer/middleware hooks
	// attach. Hooks register below before pipeline+proxy start serving.
	lifecycleReg := lifecycle.New()

	pl := &pipeline.Pipeline{
		Policy:    policySvc,
		Lifecycle: lifecycleReg,
		Logger:    slog.Default(),
		// On an upstream auth failure the agent re-resolves the key's secret
		// out-of-band (rotation), failing over without blocking when other
		// candidates exist and parking only when this key is the last resort.
		KeyAgent:   appsecret.NewAgent(keyRefresher{store: stores.HostKey, cat: cat}, 0, slog.Default()),
		HostHealth: hostHealth,
	}
	proxyPipeline := proxy.New(limiter, lifecycleReg, slog.Default())

	// Adapter specs — one Spec per supported wire shape. The composition
	// root is the only place vendor names appear; everything else looks
	// up by adapters.Name via the registry.
	openaiAuth := adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"}
	anthropicAuth := adapter.AuthStrategy{
		Header:       "x-api-key",
		ExtraHeaders: map[string]string{"anthropic-version": "2023-06-01"},
	}
	geminiAuth := adapter.AuthStrategy{Header: "x-goog-api-key"}
	// Gemini encodes the model and the sync/stream choice in the URL path
	// rather than the request body, so its upstream path is resolved per call.
	geminiUpstreamPath := func(model string, stream bool) string {
		if stream {
			return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/" + model + ":generateContent"
	}

	specs := []*adapter.Spec{
		(&adapter.Spec{
			Name: adapters.OpenAI,
			InboundPaths: []adapter.InboundPath{
				{Path: "/openai/v1/chat/completions", OperationID: "openai_chat_completions", Summary: "Create a chat completion (OpenAI Chat Completions shape)"},
			},
			UpstreamPath:  "/v1/chat/completions",
			Auth:          openaiAuth,
			Translator:    pkgopenai.CCTranslator{},
			ExtractTokens: pkgopenai.ExtractTokens,
		}).Build(),
		(&adapter.Spec{
			Name: adapters.OpenAIResponses,
			InboundPaths: []adapter.InboundPath{
				{Path: "/openai/v1/responses", OperationID: "openai_responses_create", Summary: "Create a response (OpenAI Responses API)"},
			},
			UpstreamPath:  "/v1/responses",
			Auth:          openaiAuth,
			Translator:    pkgopenai.ResponsesTranslator{},
			ExtractTokens: pkgopenai.ExtractTokens,
			UseHTTP1:      true,
			IsNativePath: func(plan *routing.Plan) bool {
				return plan.HostBinding.Spec.Adapter == adapters.OpenAI && plan.Host.Meta.Name == "openai"
			},
		}).Build(),
		(&adapter.Spec{
			Name: adapters.OpenAIEmbeddings,
			InboundPaths: []adapter.InboundPath{
				{Path: "/openai/v1/embeddings", OperationID: "openai_embeddings_create", Summary: "Create embeddings (OpenAI-compatible)"},
			},
			UpstreamPath:  "/v1/embeddings",
			Auth:          openaiAuth,
			BytePass:      true,
			ExtractTokens: pkgopenai.ExtractTokens,
		}).Build(),
		(&adapter.Spec{
			Name: adapters.Anthropic,
			InboundPaths: []adapter.InboundPath{
				{Path: "/anthropic/v1/messages", OperationID: "anthropic_messages", Summary: "Create a message (Anthropic Messages shape)"},
			},
			UpstreamPath:  "/v1/messages",
			Auth:          anthropicAuth,
			Translator:    pkganthropic.AnthropicTranslator{},
			ExtractTokens: pkganthropic.ExtractTokens,
		}).Build(),
		// Gemini native shape — upstream-only for now (HostBinding.Adapter:
		// gemini), reachable via the canonical / OpenAI / Anthropic inbound
		// shapes through the cross-shape chain. No InboundPaths yet: native
		// inbound Gemini puts the model in the URL path, which the body-based
		// minimal parse doesn't extract — a separate follow-up.
		(&adapter.Spec{
			Name:           adapters.Gemini,
			UpstreamPathFn: geminiUpstreamPath,
			Auth:           geminiAuth,
			Translator:     pkggemini.GeminiTranslator{},
			ExtractTokens:  pkggemini.ExtractTokens,
		}).Build(),
		// Canonical shape — relay's own protocol (pkg/relay/v1), served at /v1.
		// Inbound-only: callers POST canonical, relay routes + translates
		// canonical→upstream-vendor via the upstream's translator, returns
		// canonical. The identity translator makes the generic cross-shape
		// dispatch chain handle it with no special-casing.
		(&adapter.Spec{
			Name: adapters.Canonical,
			InboundPaths: []adapter.InboundPath{
				{Path: "/v1/generate", OperationID: "generate", Summary: "Generate (relay canonical shape)"},
			},
			Translator: relayv1.IdentityTranslator{},
		}).Build(),
	}
	specRegistry := adapter.NewRegistry(specs...)
	if err := specRegistry.AssertWired(); err != nil {
		slog.Error("adapter registry mis-wired", "err", err)
		os.Exit(1)
	}

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
		EnvBackend:      cfg.EventlogBackend,
		CHDSN:           cfg.CHDSN,
		PGDSN:           cfg.PGDSN,
		KV:              kvStore,
		FilePath:        usagePath,
		WALDir:          usageWALDir,
		CHRetentionDays: cfg.CHRetentionDays,
	}), slog.Default())
	defer usageCtl.Close()
	usageReader := usageCtl.Reader()
	lifecycleReg.RegisterHook(usagelog.NewUsageHook())
	lifecycleReg.RegisterCollector(usagelog.NewSinkCollector(usageCtl.Emitter()))
	lifecycleReg.RegisterStreamObserver(usagelog.NewStreamUsageFactory())
	usageCtl.Subscribe() // synchronous: register before Hydrate so the boot reload reaches it
	go usageCtl.Run(listenerCtx)
	slog.Debug("usagelog: observer wired (backend via settings: usage-logging)")

	// Payload logging: the second lifecycle observer. Always wired; its
	// runtime config lives in the "payload-logging" settings section, so it
	// toggles and reconfigures (backend / bucket / credentials) without a
	// restart. Per-request capture is still gated by the Policy/RelayKey
	// opt-in resolved at the inference entry. S3 credentials resolve through
	// the shared secret registry.
	payloadCHBootCfg := payloadCHBoot{
		DSN:           cfg.CHDSN,
		RetentionDays: 30, // payload bodies are bulkier + shorter-lived than usage rows
		WALDir:        "relay-payload-wal",
	}
	payloadCtl := payloadlog.NewController(cat, payloadSinkBuilder(stores.Secrets, payloadCHBootCfg), slog.Default())
	defer payloadCtl.Close()
	lifecycleReg.RegisterHook(payloadlog.NewPayloadHook(payloadCtl))
	lifecycleReg.RegisterCollector(payloadlog.NewSinkCollector(payloadCtl.Emitter()))
	lifecycleReg.RegisterStreamObserver(payloadlog.NewStreamPayloadFactory(payloadCtl))
	payloadCtl.Subscribe() // synchronous: register before Hydrate so the boot reload reaches it
	go payloadCtl.Run(listenerCtx)
	slog.Debug("payloadlog: observer wired (config via settings: payload-logging)")

	// Metrics: the Prometheus observer. Reads request outcome + timing in
	// post-flight and emits the request-flow metrics via pkg/metrics. Pure
	// boot wiring — no runner changes (see docs/metrics.md). The data-loss
	// and provider-key metrics emit at their sources (emitters, keypool).
	metricsObs := metricslog.New()
	lifecycleReg.RegisterPreFlight(metricsObs.PreFlight)
	lifecycleReg.RegisterHook(metricsObs)
	lifecycleReg.RegisterCollector(metricsObs)
	lifecycleReg.SetFinalizeObserver(metrics.RecordPostFlight)
	metrics.RegisterQueueDepth("usage", func() float64 { return float64(usageCtl.Emitter().QueueDepth()) })
	metrics.RegisterQueueDepth("payload", func() float64 { return float64(payloadCtl.Emitter().QueueDepth()) })
	slog.Debug("metricslog: observer wired (/metrics on control plane)")

	// Read side of payload logging: serves the /payloads/* Logs endpoints
	// over whatever backend the live settings name, rebuilt lazily on config
	// change (mirrors the sink Controller).
	payloadReader := newPayloadReaderResolver(cat, stores.Secrets, payloadCHBootCfg, slog.Default())

	// Request-parsing depth lives in the "parsing" settings section and
	// hot-swaps the openai adapter's rich-parse toggle. The vendor setter
	// is confined here (composition root) so app/ stays vendor-neutral.
	settingswatch.New(cat, settings.SectionParsing, func(p settings.Parsing) {
		pkgopenai.SetRichParsing(p.RichParsing)
		slog.Debug("parsing: applied", "rich_parsing", p.RichParsing)
	}, slog.Default()).Start()

	// All settings-change subscribers are now registered; start background
	// hydration. Its first Hydrate runs settings.reload → notifies them with
	// the stored values (the data plane gates on IsReady until it completes).
	go hydrateLoop(listenerCtx, cat, stores, bootOpts)

	// Batch subsystem: jobq-backed background execution of bulk inference
	// submissions. jobq owns durable per-item execution + payload storage;
	// app/batch owns the batch record and the customer API. The per-item
	// handler reuses the same routing + pipeline as the realtime path.
	if err := jobq.Migrate(bootCtx, st.Pool()); err != nil {
		slog.Error("jobq migrate failed", "err", err)
		os.Exit(1)
	}
	batchPayloadDir := os.Getenv("RELAY_BATCH_PAYLOAD_DIR")
	if batchPayloadDir == "" {
		batchPayloadDir = "relay-batch-payloads"
	}
	batchPayloads, err := payload.NewFileStore(batchPayloadDir)
	if err != nil {
		slog.Error("batch payload store init failed", "err", err)
		os.Exit(1)
	}
	batchQueue := jobq.New(st.Pool(), batchPayloads, jobq.Options{})
	batchSvc := batch.NewService(
		batch.NewStore(st.Pool()),
		batchQueue,
		&batch.Runner{Resolver: routing.New(cat), Pipeline: pl, Specs: specRegistry, Catalog: cat},
	)
	batchQueue.Register(batch.Queue, batchSvc.Handler())
	if err := batchQueue.Start(listenerCtx); err != nil {
		slog.Error("batch queue start failed", "err", err)
		os.Exit(1)
	}
	slog.Info("batch: subsystem started", "payload_dir", batchPayloadDir)

	// Inference plane (data plane): /v1/*, /healthz on RELAY_PORT.
	inferRouter := chi.NewRouter()
	inferRouter.Use(reqid.Middleware(slog.Default()))
	inference.Mount(inferRouter, inference.Deps{
		Pinger:         st,
		Catalog:        cat,
		Resolver:       routing.New(cat),
		Pipeline:       pl,
		Proxy:          proxyPipeline,
		Lifecycle:      lifecycleReg,
		Adapters:       specRegistry.AdapterMap(),
		Specs:          specRegistry,
		RouteMounters:  []inference.RouteMounter{inference.MountRegistry(specRegistry)},
		TrustEventTime: cfg.DevTrustEventTime,
	})

	// /v1/batches rides the same auth chain as /v1/* (readiness → classify →
	// relay-key auth), mounted directly on chi like /v1/ws since it isn't a
	// huma operation.
	inferRouter.With(
		inference.ReadinessMiddleware(cat),
		inference.ClassifyMiddleware(),
		inference.RelayKeyAuthMiddleware(cat),
	).Mount("/v1/batches", batchSvc.Routes())

	inferAddr := ":8080"
	if p := os.Getenv("RELAY_PORT"); p != "" {
		inferAddr = ":" + p
	}
	inferSrv := &http.Server{Addr: inferAddr, Handler: inferRouter}
	slog.Info("relay inference listening", "addr", inferAddr)
	inferErr := make(chan error, 1)
	go func() { inferErr <- inferSrv.ListenAndServe() }()

	// Control plane (admin plane): /auth/*, CRUD, /version, /reload on
	// RELAY_CONTROL_PORT. Disabled when empty or "off".
	var ctrlSrv *http.Server
	if cfg.ControlPort != "" && cfg.ControlPort != "off" {
		ctrlRouter := chi.NewRouter()
		if len(cfg.ControlAllowOrigins) > 0 {
			ctrlRouter.Use(control.CORS(cfg.ControlAllowOrigins...))
		}
		control.Mount(ctrlRouter, control.Deps{
			Identity:      idStore,
			Sessions:      sessMgr,
			AdminToken:    cfg.AdminToken,
			Authz:         authz.AlwaysAllowAuthenticated{},
			Catalog:       cat,
			Stores:        stores,
			CookieSecure:  cookieSecure,
			UsageReader:   usageReader,
			PayloadReader: payloadReader,
			Selector:      selector,
			HostHealth:    hostHealth,
			RuntimeConfig: runtimeConfig(cfg),
		})
		ctrlRouter.Handle("/metrics", metrics.Handler())
		// Embedded admin UI: same-origin SPA served as the fallback after all
		// API operations. Only mounted when a real dist was baked in (image
		// build) and not explicitly disabled.
		if !cfg.UIDisable && relayweb.Present() {
			ctrlRouter.NotFound(relayweb.Handler().ServeHTTP)
			slog.Debug("relay control: serving embedded UI")
		}
		ctrlSrv = &http.Server{Addr: ":" + cfg.ControlPort, Handler: ctrlRouter}
		slog.Info("relay control listening", "addr", ctrlSrv.Addr, "users", len(idStore.Users()))
		go func() {
			if err := ctrlSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("relay control: server error", "err", err)
			}
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-quit:
		slog.Info("relay: received signal, shutting down", "signal", sig.String())
	case err := <-inferErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("relay inference: server error", "err", err)
		}
	}

	deadline := time.Duration(cfg.ShutdownDeadlineS) * time.Second
	if deadline == 0 {
		deadline = 15 * time.Second
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), deadline)
	defer shutCancel()
	if ctrlSrv != nil {
		_ = ctrlSrv.Shutdown(shutCtx)
	}
	_ = inferSrv.Shutdown(shutCtx)
	cancelListener()
}

// hydrateLoop runs Catalog.Hydrate with exponential backoff until it
// succeeds, then starts the NOTIFY listener. Survives transient PG /
// seed errors without taking the process down; the data plane returns
// 503 until the first Hydrate completes. Once successful, the function
// blocks on Listener.Run until the parent context is cancelled.
func hydrateLoop(ctx context.Context, cat *appcatalog.Catalog, stores *appcatalog.Stores, opts appcatalog.BootstrapOptions) {
	delay := time.Second
	const maxDelay = 30 * time.Second
	for {
		listener, err := cat.Hydrate(ctx, stores, opts)
		if err == nil {
			slog.Info("catalog hydrated", "auto_seed_dir", opts.AutoSeedDir)
			if err := listener.Run(ctx); err != nil && err != context.Canceled {
				slog.Error("catalog listener exited", "err", err)
			}
			return
		}
		slog.Error("catalog hydrate failed; retrying", "err", err, "delay", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// loadDotEnv reads a .env file and sets any KEY=VALUE pair whose key is not
// already present in the environment. Comment lines and empty lines are skipped.
// logLevel reads RELAY_LOG_LEVEL (debug|info|warn|error, default info). Parsed
// here rather than via config.Load because the logger is set up before config.
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// catalogSnapReader adapts *appcatalog.Catalog to policy.SnapshotReader by
// reading the current snapshot per lookup, so each call sees the latest
// post-NOTIFY state.
type catalogSnapReader struct{ cat *appcatalog.Catalog }

func (r catalogSnapReader) Policy(id string) (*policy.Policy, bool) {
	return r.cat.Current().Policy(id)
}

func (r catalogSnapReader) RateLimit(id string) (*ratelimit.RateLimit, bool) {
	return r.cat.Current().RateLimit(id)
}

// keyRefresher implements appsecret.Refresher. It re-resolves a host key's
// secret from its backend (hostkey.Store.Get re-runs the secret.Ref through
// the registry) and, if the value changed, heals the live snapshot via the
// normal apply path — the same machinery catalog NOTIFY uses. Reused by the
// runtimeConfig maps the parsed env (config.RuntimeConfig) into the control
// plane's GET /config.json body, keeping the config package free of the
// httpapi/control type. Telemetry is omitted entirely unless a DSN is set.
func runtimeConfig(cfg *config.Config) control.RuntimeConfig {
	rc := control.RuntimeConfig{
		ControlAPIURL:   cfg.Runtime.ControlAPIURL,
		InferenceAPIURL: cfg.Runtime.InferenceAPIURL,
		Mode:            cfg.Runtime.Mode,
		DocsURL:         cfg.Runtime.DocsURL,
		SupportURL:      cfg.Runtime.SupportURL,
	}
	if cfg.Runtime.SentryDSN != "" {
		rc.Telemetry = &control.Telemetry{
			SentryDSN:   cfg.Runtime.SentryDSN,
			Environment: cfg.Runtime.TelemetryEnv,
		}
	}
	return rc
}

// KeyAgent to recover from upstream key rotation without a restart.
type keyRefresher struct {
	store *hostkey.Store
	cat   *appcatalog.Catalog
}

func (r keyRefresher) Refresh(ctx context.Context, keyID string) (string, bool, error) {
	k, err := r.store.Get(ctx, keyID)
	if err != nil || k == nil {
		return "", false, err
	}
	cur, ok := r.cat.Current().HostKey(keyID)
	changed := !ok || cur.Resolved != k.Resolved
	if changed {
		if err := r.cat.ApplyHostKeyUpsert(k); err != nil {
			return k.Resolved, true, err
		}
	}
	return k.Resolved, changed, nil
}
