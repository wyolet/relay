package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	apianthropic "github.com/wyolet/relay/internal/api/anthropic"
	apiopenai "github.com/wyolet/relay/internal/api/openai"
	pkganthropic "github.com/wyolet/relay/pkg/api/anthropic"
	pkgopenai "github.com/wyolet/relay/pkg/api/openai"
	"github.com/wyolet/relay/internal/auth"
	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/keypool"
	"github.com/wyolet/relay/internal/pipeline"
	"github.com/wyolet/relay/internal/provider"
	provideranthropicpkg "github.com/wyolet/relay/internal/provider/anthropic"
	"github.com/wyolet/relay/internal/provider/ollama"
	"github.com/wyolet/relay/internal/provider/openai"
	"github.com/wyolet/relay/internal/ratelimit"
	"github.com/wyolet/relay/internal/routing"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/usage"
	"github.com/wyolet/relay/pkg/eventlog"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/transport"
)

// masterKey holds the parsed RELAY_MASTER_KEY (32 bytes) or nil if unset.
// PER-266 consumers read this at construction time.
var masterKey []byte

// pinger is anything that can report its own health.
type pinger interface {
	Ping(ctx context.Context) error
}

// healthzHandler builds a GET /healthz handler given named pingers.
// Backends with a nil pinger are reported "ok" unconditionally (memory/file).
func healthzHandler(backends map[string]pinger, deadlineMS int, masterKeyConfigured bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dl := time.Duration(deadlineMS) * time.Millisecond
		ctx, cancel := context.WithTimeout(r.Context(), dl)
		defer cancel()

		type result struct {
			name   string
			status string
		}
		results := make(chan result, len(backends))
		var wg sync.WaitGroup

		for name, p := range backends {
			name, p := name, p
			wg.Add(1)
			go func() {
				defer wg.Done()
				if p == nil {
					results <- result{name, "ok"}
					return
				}
				if err := p.Ping(ctx); err != nil {
					slog.Warn("healthz: backend error", "backend", name, "err", err)
					results <- result{name, "error: " + err.Error()}
					return
				}
				results <- result{name, "ok"}
			}()
		}

		wg.Wait()
		close(results)

		overall := "ok"
		backendsOut := make(map[string]string, len(backends))
		for r := range results {
			backendsOut[r.name] = r.status
			if r.status != "ok" {
				overall = "degraded"
			}
		}

		code := http.StatusOK
		if overall != "ok" {
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":                overall,
			"backends":              backendsOut,
			"master_key_configured": masterKeyConfigured,
		})
	}
}

func main() {
	loadDotEnv(".env")
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			runMigrate(os.Args[2:])
			return
		case "seed":
			runSeed(os.Args[2:])
			return
		case "master-key":
			runMasterKey(os.Args[2:])
			return
		case "import":
			runImport(os.Args[2:])
			return
		}
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config invalid", "err", err)
		os.Exit(1)
	}

	// Apply settings
	pkgopenai.SetRichParsing(cfg.RichParsing)
	masterKey = cfg.MasterKey

	bootCtx := context.Background()

	// Event log — BackendFile by default; BackendClickHouse when cfg.EventlogBackend=clickhouse.
	elCfg := eventlog.Config{
		Dir: cfg.EventlogDir,
	}
	if cfg.EventlogBackend == "clickhouse" {
		elCfg.Backend = eventlog.BackendClickHouse
		elCfg.DSN = cfg.CHDSN
		if elCfg.DSN == "" {
			slog.Error("RELAY_CH_DSN not set (required when RELAY_EVENTLOG_BACKEND=clickhouse)")
			os.Exit(1)
		}
		if cfg.CHRetentionDays > 0 {
			elCfg.RetentionDays = cfg.CHRetentionDays
		}
	}
	el, err := eventlog.New(elCfg)
	if err != nil {
		slog.Error("eventlog init failed", "err", err)
		os.Exit(1)
	}

	// OTel TracerProvider — no-op when RELAY_OTLP_ENDPOINT is unset.
	usageShutdown, err := usage.Init(bootCtx, usage.Config{
		OTLPEndpoint:    cfg.OTLPEndpoint,
		EventLog:        el,
		CatalogBackend:  cfg.CatalogBackend,
		StateBackend:    cfg.StateBackend,
		EventlogBackend: cfg.EventlogBackend,
		InstanceID:      cfg.InstanceID,
	})
	if err != nil {
		log.Fatalf("usage.Init: %v", err)
	}

	var catalogStore catalog.Store
	var pgStoreForAdmin *catalog.PGStore
	var storageForAdmin *storagemod.Storage
	var pgPinger pinger
	if cfg.CatalogBackend == "pg" {
		if cfg.PGDSN == "" {
			slog.Error("RELAY_PG_DSN not set (required when RELAY_CATALOG_BACKEND=pg)")
			os.Exit(1)
		}
		// Open storage first (runs migrations), then optionally auto-seed.
		st, err := storagemod.Open(bootCtx, cfg.PGDSN)
		if err != nil {
			slog.Error("storage.Open failed", "err", err)
			os.Exit(1)
		}
		pgStore, err := catalog.NewPGStore(st.Catalog, st, masterKey)
		if err != nil {
			st.Close()
			slog.Error("configstore(pg) init failed", "err", err)
			os.Exit(1)
		}
		pgStoreForAdmin = pgStore
		storageForAdmin = st
		catalogStore = pgStore
		pgPinger = pgStore

		// Auto-seed from config dir if DB is empty (RELAY_AUTO_SEED_IF_EMPTY=1).
		if cfg.AutoSeedIfEmpty {
			if err := maybeAutoSeed(bootCtx, cfg.PGDSN, cfg.ConfigDir); err != nil {
				log.Fatalf("auto-seed: %v", err)
			}
		}

		// Seed bundled default providers (openai, ollama) on first launch.
		// No-op once the operator has created any provider.
		if err := seedDefaultProviders(bootCtx, pgStore, st); err != nil {
			slog.Warn("default provider seed failed", "err", err)
		}

		// Cluster mode: subscribe to PG NOTIFY relay_catalog so that catalog
		// writes on any pod fan out to all other pods within ~100ms.
		// The NOTIFY producer (in storage/catalog.go) is unconditional; only
		// the LISTEN consumer is gated here.
		if cfg.ClusterMode {
			watcher, err := storagemod.NewCatalogWatcher(bootCtx, cfg.PGDSN, func() {
				if err := pgStore.Reload(bootCtx); err != nil {
					slog.Warn("cluster: catalog reload after NOTIFY failed", "err", err)
				}
			}, slog.Default())
			if err != nil {
				slog.Error("cluster: NewCatalogWatcher failed", "err", err)
				os.Exit(1)
			}
			defer watcher.Close()
			slog.Info("cluster mode enabled: subscribed to relay_catalog NOTIFY")
		}
	} else {
		yamlStore, err := catalog.LoadYAML(cfg.ConfigDir)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		catalogStore = yamlStore
	}

	// State store — Redis when RELAY_STATE_BACKEND=redis, else in-memory.
	var st kv.Store
	var redisPinger pinger
	if cfg.StateBackend == "redis" {
		if cfg.RedisAddr == "" {
			slog.Error("RELAY_REDIS_ADDR not set (required when RELAY_STATE_BACKEND=redis)")
			os.Exit(1)
		}
		rs, err := kv.NewRedis(bootCtx, kv.RedisConfig{Addr: cfg.RedisAddr})
		if err != nil {
			slog.Error("state(redis) init failed", "err", err)
			os.Exit(1)
		}
		st = rs
		redisPinger = rs
	} else {
		st = kv.NewMem()
	}

	limiter := ratelimit.New(st, slog.Default(), nil)
	sel := keypool.New(st, slog.Default(), nil, limiter, catalogStore, nil)

	reg := provider.NewRegistry()
	for _, p := range catalogStore.Providers() {
		switch p.Spec.Kind {
		case catalog.PKOllama:
			reg.Register(catalog.PKOllama, p.Spec.BaseURL, ollama.New(p.Spec.BaseURL))
		case catalog.PKOpenAI:
			baseURL := p.Spec.BaseURL
			if baseURL == "" {
				baseURL = "https://api.openai.com"
			}
			reg.Register(catalog.PKOpenAI, baseURL, openai.New(baseURL))
		case catalog.PKAnthropic:
			baseURL := p.Spec.BaseURL
			if baseURL == "" {
				baseURL = "https://api.anthropic.com"
			}
			reg.RegisterMessages(catalog.PKAnthropic, baseURL, provideranthropicpkg.New(baseURL))
		}
	}

	// normalizeBaseURL returns the canonical baseURL for a provider kind.
	normalizeBaseURL := func(kind catalog.ProviderKind, baseURL string) string {
		if baseURL == "" {
			switch kind {
			case catalog.PKOpenAI:
				return "https://api.openai.com"
			case catalog.PKAnthropic:
				return "https://api.anthropic.com"
			}
		}
		return baseURL
	}

	// outboundFor resolves the provider adapter for a request plan.
	// The registry is keyed by (Kind, BaseURL): a baseURL change in the catalog
	// produces a cache miss, so the updated client is created and cached on the
	// next request without any explicit invalidation.
	outboundFor := func(plan *apiopenai.RequestPlan) (provider.Outbound, error) {
		baseURL := normalizeBaseURL(plan.Provider.Spec.Kind, plan.Provider.Spec.BaseURL)
		ob, err := reg.Get(plan.Provider.Spec.Kind, baseURL)
		if err == nil {
			return ob, nil
		}
		// Not yet registered — create and cache it now.
		switch plan.Provider.Spec.Kind {
		case catalog.PKOllama:
			ob = ollama.New(baseURL)
		case catalog.PKOpenAI:
			ob = openai.New(baseURL)
		default:
			return nil, fmt.Errorf("provider: no outbound registered for kind %q", plan.Provider.Spec.Kind)
		}
		reg.Register(plan.Provider.Spec.Kind, baseURL, ob)
		return ob, nil
	}

	// messagesOutboundFor resolves a MessagesOutbound for an Anthropic plan.
	messagesOutboundFor := func(plan *apianthropic.RequestPlan) (provider.MessagesOutbound, error) {
		baseURL := normalizeBaseURL(plan.Provider.Spec.Kind, plan.Provider.Spec.BaseURL)
		ob, err := reg.GetMessages(plan.Provider.Spec.Kind, baseURL)
		if err == nil {
			return ob, nil
		}
		// Not yet registered — create and cache it now.
		if plan.Provider.Spec.Kind == catalog.PKAnthropic {
			client := provideranthropicpkg.New(baseURL)
			reg.RegisterMessages(catalog.PKAnthropic, baseURL, client)
			return client, nil
		}
		return nil, fmt.Errorf("provider: no messages outbound for kind %q", plan.Provider.Spec.Kind)
	}

	resolver := routing.New(catalogStore)

	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *apiopenai.RequestPlan) (pipeline.RunResult, error) {
		ob, err := outboundFor(plan)
		if err != nil {
			return pipeline.RunResult{}, err
		}
		if plan.Pool != nil && len(plan.Secrets) > 0 {
			return pipeline.Run(ctx, ch, pipeline.RunOptions{
				Provider:       plan.Provider,
				Pool:           plan.Pool,
				Model:          plan.Model,
				Secrets:        plan.Secrets,
				Selector:       sel,
				Outbound:       ob,
				Limiter:        limiter,
				Rules:          plan.Rules,
				TokenExtractor: pkgopenai.ExtractTokens,
				CatalogStore:   catalogStore,
			})
		}
		emptySecret := &catalog.Secret{
			Metadata: catalog.Metadata{Name: "anon"},
			Resolved: "",
			KeyHash:  "anon",
		}
		syntheticPool := &catalog.Pool{
			Metadata: catalog.Metadata{Name: "anon-pool"},
		}
		return pipeline.Run(ctx, ch, pipeline.RunOptions{
			Pool:           syntheticPool,
			Secrets:        []*catalog.Secret{emptySecret},
			Selector:       sel,
			Outbound:       ob,
			TokenExtractor: pkgopenai.ExtractTokens,
		})
	}

	runAnthropicPipeline := func(ctx context.Context, ch *transport.Channel, plan *apianthropic.RequestPlan) (pipeline.RunResult, error) {
		mob, err := messagesOutboundFor(plan)
		if err != nil {
			return pipeline.RunResult{}, err
		}
		// Inject per-request extras (query string, passthrough headers) via context.
		if plan.RawQuery != "" || len(plan.PassthroughHeaders) > 0 {
			ctx = provideranthropicpkg.WithRequestExtras(ctx, provideranthropicpkg.RequestExtras{
				RawQuery:     plan.RawQuery,
				ExtraHeaders: plan.PassthroughHeaders,
			})
		}
		doUpstream := mob.Messages
		if plan.Pool != nil && (len(plan.Secrets) > 0 || plan.Passthrough) {
			return pipeline.Run(ctx, ch, pipeline.RunOptions{
				Provider:        plan.Provider,
				Pool:            plan.Pool,
				Model:           plan.Model,
				Secrets:         plan.Secrets,
				Selector:        sel,
				DoUpstream:      doUpstream,
				Limiter:         limiter,
				Rules:           plan.Rules,
				TokenExtractor:  pkganthropic.ExtractTokens,
				CatalogStore:    catalogStore,
				PassthroughAuth: plan.PassthroughAuth,
			})
		}
		emptySecret := &catalog.Secret{
			Metadata: catalog.Metadata{Name: "anon"},
			Resolved: "",
			KeyHash:  "anon",
		}
		syntheticPool := &catalog.Pool{
			Metadata: catalog.Metadata{Name: "anon-pool"},
		}
		return pipeline.Run(ctx, ch, pipeline.RunOptions{
			Pool:           syntheticPool,
			Secrets:        []*catalog.Secret{emptySecret},
			Selector:       sel,
			DoUpstream:     doUpstream,
			TokenExtractor: pkganthropic.ExtractTokens,
		})
	}

	// Healthcheck backends: nil pinger = unconditionally healthy (memory/file).
	healthzBackends := map[string]pinger{
		"catalog":  pgPinger,    // nil when yaml
		"state":    redisPinger, // nil when memory
		"eventlog": el,          // always non-nil; fileSink.ping returns nil
	}

	maxReqBytes := cfg.MaxRequestBytes
	if maxReqBytes == 0 {
		maxReqBytes = httpmw.DefaultMaxRequestBytes
	}

	apiKeys := cfg.APIKeys
	if len(apiKeys) == 0 {
		slog.Warn("auth: no API keys configured — running fail-open (RELAY_API_KEY/RELAY_API_KEYS unset)")
	}
	authMW := auth.Middleware(apiKeys)

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(maxReqBytes))

	var adminH http.HandlerFunc
	var adminCRUDHandlers *adminCRUD
	if cfg.AdminToken != "" && pgStoreForAdmin != nil {
		adminH = adminReloadHandler(cfg.AdminToken, pgStoreForAdmin, limiter, cfg.AdminReloadRPM)

		deps := crudDeps(storageForAdmin, pgStoreForAdmin)
		kinds := buildAdminKinds(pgStoreForAdmin, storageForAdmin)
		adminCRUDHandlers = buildAdminCRUD(kinds, deps, pgStoreForAdmin, st)
	}

	// Mount huma on the top-level chi router. It registers /openapi.json, /docs,
	// /schemas (unauthenticated) and all business-logic operations (auth enforced
	// per-operation via humaAuth). The chi Group pattern from PER-249 is replaced
	// by per-op huma middleware; auth_wiring_test.go uses its own plain chi helper.
	mountHuma(
		r,
		authMW,
		healthzHandler(healthzBackends, cfg.HealthzDeadlineMS, len(masterKey) > 0),
		apiopenai.ChatCompletions(resolver, runPipeline),
		apiopenai.ListModels(catalogStore),
		apianthropic.MessagesHandler(resolver, runAnthropicPipeline),
		adminH,
		adminCRUDHandlers,
		cfg.AdminToken,
	)

	// Mount the operator admin UI at /ui (unauthenticated static assets; PER-274 gates API calls).
	mountUI(r)

	// Prometheus metrics endpoint. No auth — scrapers reach this over the cluster
	// network; protecting /metrics is a deployment concern, not application concern.
	r.Method(http.MethodGet, "/metrics", metrics.Handler())

	addr := ":8080"
	srv := &http.Server{Addr: addr, Handler: r}

	slog.Info("relay listening", "addr", addr)
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe() }()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-quit:
		slog.Info("relay: received signal, shutting down", "signal", sig.String())
	case err := <-srvErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("relay: server error", "err", err)
		}
	}

	totalDeadline := time.Duration(cfg.ShutdownDeadlineS) * time.Second
	shutCtx, shutCancel := context.WithTimeout(context.Background(), totalDeadline)
	defer shutCancel()
	shutdown(shutCtx, srv, usageShutdown, el, st, catalogStore)
}

// shutdown executes the ordered drain sequence within the provided context deadline.
func shutdown(
	ctx context.Context,
	srv *http.Server,
	usageShutdown usage.Shutdown,
	el *eventlog.Logger,
	st kv.Store,
	cfg catalog.Store,
) {
	step := func(name string, fn func(context.Context) error, budget time.Duration) {
		slog.Info("shutdown: starting step", "step", name)
		stepCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		if err := fn(stepCtx); err != nil {
			slog.Warn("shutdown: step exceeded deadline or errored", "step", name, "err", err)
			return
		}
		slog.Info("shutdown: step done", "step", name)
	}

	// 1. Stop accepting new HTTP requests (10s of the budget).
	step("http", func(ctx context.Context) error { return srv.Shutdown(ctx) }, 10*time.Second)

	// 2. Drain OTel batch processor (5s).
	step("usage", usageShutdown, 5*time.Second)

	// 3. Flush pending eventlog inserts (remaining deadline via ctx).
	step("eventlog", func(ctx context.Context) error { return el.Close(ctx) }, 8*time.Second)

	// 4. Drain in-flight Lua scripts.
	step("state", func(_ context.Context) error { return st.Close() }, 5*time.Second)

	// 5. Close the storage pool.
	step("configstore", func(_ context.Context) error {
		if pg, ok := cfg.(*catalog.PGStore); ok {
			pg.Close()
		}
		return nil
	}, 2*time.Second)
}
