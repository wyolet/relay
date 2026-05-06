package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	apiopenai "github.com/wyolet/relay/pkg/api/openai"
	"github.com/wyolet/relay/pkg/auth"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/eventlog"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/keypool"
	"github.com/wyolet/relay/pkg/limit"
	"github.com/wyolet/relay/pkg/pipeline"
	"github.com/wyolet/relay/pkg/provider"
	"github.com/wyolet/relay/pkg/provider/ollama"
	"github.com/wyolet/relay/pkg/provider/openai"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/state"
	"github.com/wyolet/relay/pkg/transport"
	"github.com/wyolet/relay/pkg/usage"
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
func healthzHandler(backends map[string]pinger, deadlineMS int) http.HandlerFunc {
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
			"status":   overall,
			"backends": backendsOut,
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
		}
	}

	// Parse RELAY_MASTER_KEY if present. Optional — env-ref secrets work without it.
	// PER-266 consumers will read masterKey.
	if raw := os.Getenv("RELAY_MASTER_KEY"); raw != "" {
		var err error
		masterKey, err = crypto.ParseMasterKey(raw)
		if err != nil {
			slog.Error("RELAY_MASTER_KEY invalid", "err", err)
			os.Exit(1)
		}
	}

	// RELAY_RICH_PARSING: "on" (default) enables full body parse to ChatRequest.
	// "off" reverts to minimal-parse (model/stream/user/raw only, metadata ignored).
	// Any other value exits with a clear error.
	switch rp := os.Getenv("RELAY_RICH_PARSING"); rp {
	case "", "on":
		apiopenai.SetRichParsing(true)
	case "off":
		apiopenai.SetRichParsing(false)
	default:
		slog.Error("RELAY_RICH_PARSING must be \"on\" or \"off\"", "got", rp)
		os.Exit(1)
	}

	bootCtx := context.Background()

	catalogBackend := os.Getenv("RELAY_CATALOG_BACKEND")
	if catalogBackend == "" {
		catalogBackend = "yaml"
	}
	stateBackend := os.Getenv("RELAY_STATE_BACKEND")
	if stateBackend == "" {
		stateBackend = "memory"
	}
	eventlogBackend := os.Getenv("RELAY_EVENTLOG_BACKEND")
	if eventlogBackend == "" {
		eventlogBackend = "file"
	}

	// Event log — BackendFile by default; BackendClickHouse when RELAY_EVENTLOG_BACKEND=clickhouse.
	elCfg := eventlog.Config{}
	if eventlogBackend == "clickhouse" {
		elCfg.Backend = eventlog.BackendClickHouse
		elCfg.DSN = os.Getenv("RELAY_CH_DSN")
		if elCfg.DSN == "" {
			slog.Error("RELAY_CH_DSN not set (required when RELAY_EVENTLOG_BACKEND=clickhouse)")
			os.Exit(1)
		}
		if days := envInt("RELAY_CH_RETENTION_DAYS", 90); days > 0 {
			elCfg.RetentionDays = days
		}
	}
	el, err := eventlog.New(elCfg)
	if err != nil {
		slog.Error("eventlog init failed", "err", err)
		os.Exit(1)
	}

	// OTel TracerProvider — no-op when RELAY_OTLP_ENDPOINT is unset.
	usageShutdown, err := usage.Init(bootCtx, usage.Config{
		OTLPEndpoint:    os.Getenv("RELAY_OTLP_ENDPOINT"),
		EventLog:        el,
		CatalogBackend:  catalogBackend,
		StateBackend:    stateBackend,
		EventlogBackend: eventlogBackend,
	})
	if err != nil {
		log.Fatalf("usage.Init: %v", err)
	}

	var cfg configstore.ConfigStore
	var pgStoreForAdmin *configstore.PGStore
	var pgPinger pinger
	if catalogBackend == "pg" {
		pgDSN := os.Getenv("RELAY_PG_DSN")
		if pgDSN == "" {
			slog.Error("RELAY_PG_DSN not set (required when RELAY_CATALOG_BACKEND=pg)")
			os.Exit(1)
		}
		autoSeed := os.Getenv("RELAY_AUTO_SEED_IF_EMPTY") == "1"
		if autoSeed {
			if err := maybeAutoSeed(bootCtx, pgDSN); err != nil {
				log.Fatalf("auto-seed: %v", err)
			}
		}
		pgStore, err := configstore.Postgres(bootCtx, pgDSN, masterKey)
		if err != nil {
			slog.Error("configstore(pg) init failed", "err", err)
			os.Exit(1)
		}
		pgStoreForAdmin = pgStore
		cfg = pgStore
		pgPinger = pgStore
	} else {
		yamlStore, err := configstore.LoadYAML("config")
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		cfg = yamlStore
	}

	// State store — Redis when RELAY_STATE_BACKEND=redis, else in-memory.
	var st state.Store
	var redisPinger pinger
	if stateBackend == "redis" {
		addr := os.Getenv("RELAY_REDIS_ADDR")
		if addr == "" {
			slog.Error("RELAY_REDIS_ADDR not set (required when RELAY_STATE_BACKEND=redis)")
			os.Exit(1)
		}
		rs, err := state.NewRedis(bootCtx, state.RedisConfig{Addr: addr})
		if err != nil {
			slog.Error("state(redis) init failed", "err", err)
			os.Exit(1)
		}
		st = rs
		redisPinger = rs
	} else {
		st = state.New()
	}

	limiter := limit.New(st, slog.Default(), nil)
	sel := keypool.New(st, slog.Default(), nil, limiter, cfg, nil)

	reg := provider.NewRegistry()
	for _, p := range cfg.Providers() {
		switch p.Spec.Kind {
		case configstore.PKOllama:
			reg.Register(configstore.PKOllama, ollama.New(p.Spec.BaseURL))
		case configstore.PKOpenAI:
			baseURL := p.Spec.BaseURL
			if baseURL == "" {
				baseURL = "https://api.openai.com"
			}
			reg.Register(configstore.PKOpenAI, openai.New(baseURL))
		}
	}

	resolve := func(modelName string) (*apiopenai.RequestPlan, bool) {
		m, ok := cfg.ModelByName(modelName)
		if !ok {
			return nil, false
		}
		p, ok := cfg.ProviderForModel(modelName)
		if !ok {
			return nil, false
		}
		plan := &apiopenai.RequestPlan{Model: m, Provider: p}
		if poolName := p.Spec.DefaultPool; poolName != "" {
			if pool, ok := cfg.PoolByName(poolName); ok {
				plan.Pool = pool
				plan.Secrets = cfg.SecretsForPool(pool)
				plan.Rules = cfg.RateLimitsForRequest(p, pool, m, nil)
			}
		}
		return plan, true
	}

	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *apiopenai.RequestPlan) error {
		ob, err := reg.Get(plan.Provider.Spec.Kind)
		if err != nil {
			return err
		}
		if plan.Pool != nil && len(plan.Secrets) > 0 {
			return pipeline.Run(ctx, ch, pipeline.RunOptions{
				Provider: plan.Provider,
				Pool:     plan.Pool,
				Model:    plan.Model,
				Secrets:  plan.Secrets,
				Selector: sel,
				Outbound: ob,
				Limiter:  limiter,
				Rules:    plan.Rules,
			})
		}
		emptySecret := &configstore.Secret{
			Metadata: configstore.Metadata{Name: "anon"},
			Resolved: "",
			KeyHash:  "anon",
		}
		syntheticPool := &configstore.Pool{
			Metadata: configstore.Metadata{Name: "anon-pool"},
		}
		return pipeline.Run(ctx, ch, pipeline.RunOptions{
			Pool:     syntheticPool,
			Secrets:  []*configstore.Secret{emptySecret},
			Selector: sel,
			Outbound: ob,
		})
	}

	// Healthcheck backends: nil pinger = unconditionally healthy (memory/file).
	healthzDeadlineMS := envInt("RELAY_HEALTHZ_DEADLINE_MS", 500)
	healthzBackends := map[string]pinger{
		"catalog":  pgPinger,    // nil when yaml
		"state":    redisPinger, // nil when memory
		"eventlog": el,          // always non-nil; fileSink.ping returns nil
	}

	apiKeys := auth.ParseKeys(os.Getenv("RELAY_API_KEY"), os.Getenv("RELAY_API_KEYS"))
	if len(apiKeys) == 0 {
		slog.Warn("auth: no API keys configured — running fail-open (RELAY_API_KEY/RELAY_API_KEYS unset)")
	}
	authMW := auth.Middleware(apiKeys)

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.MaxRequestBytesFromEnv()))

	var adminH http.HandlerFunc
	var adminCRUDHandlers *adminCRUD
	if tok := os.Getenv("RELAY_ADMIN_TOKEN"); tok != "" && pgStoreForAdmin != nil {
		adminH = adminReloadHandler(tok, pgStoreForAdmin, limiter)

		deps := crudDeps(pgStoreForAdmin.RawPool(), pgStoreForAdmin)
		kinds := buildAdminKinds(pgStoreForAdmin, nil)
		adminCRUDHandlers = buildAdminCRUD(kinds, deps)
		mountAdminRoutes(r, tok, adminCRUDHandlers)
	}

	// Mount huma on the top-level chi router. It registers /openapi.json, /docs,
	// /schemas (unauthenticated) and all business-logic operations (auth enforced
	// per-operation via humaAuth). The chi Group pattern from PER-249 is replaced
	// by per-op huma middleware; auth_wiring_test.go uses its own plain chi helper.
	mountHuma(
		r,
		authMW,
		healthzHandler(healthzBackends, healthzDeadlineMS),
		apiopenai.ChatCompletions(resolve, runPipeline),
		apiopenai.ListModels(cfg),
		adminH,
		adminCRUDHandlers,
	)

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

	totalDeadline := time.Duration(envInt("RELAY_SHUTDOWN_DEADLINE_S", 15)) * time.Second
	shutCtx, shutCancel := context.WithTimeout(context.Background(), totalDeadline)
	defer shutCancel()
	shutdown(shutCtx, srv, usageShutdown, el, st, cfg)
}

// shutdown executes the ordered drain sequence within the provided context deadline.
func shutdown(
	ctx context.Context,
	srv *http.Server,
	usageShutdown usage.Shutdown,
	el *eventlog.Logger,
	st state.Store,
	cfg configstore.ConfigStore,
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

	// 5. Close pgxpool.
	step("configstore", func(_ context.Context) error {
		if pg, ok := cfg.(*configstore.PGStore); ok {
			pg.Close()
		}
		return nil
	}, 2*time.Second)
}
