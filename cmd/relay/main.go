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
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/identity"
	storagemod "github.com/wyolet/relay/internal/storage"
	pkganthropic "github.com/wyolet/relay/pkg/adapters/anthropic"
	pkggemini "github.com/wyolet/relay/pkg/adapters/gemini"
	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	relayv1 "github.com/wyolet/relay/pkg/relay/v1"
	"github.com/wyolet/relay/pkg/reqid"
)

func main() {
	loadDotEnv(".env")
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			slog.Info("relay: 'migrate' subcommand currently runs implicitly on boot")
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

	listenerCtx, cancelListener := context.WithCancel(bootCtx)
	defer cancelListener()
	go hydrateLoop(listenerCtx, cat, stores, bootOpts)

	// Identity store — fatal if YAML is malformed (login would silently
	// be disabled otherwise). Empty store is fine (login returns 503).
	idStore, err := identity.LoadYAML(cfg.ConfigDir)
	if err != nil {
		slog.Error("identity: load YAML failed", "err", err)
		os.Exit(1)
	}
	if n := len(idStore.Users()); n > 0 {
		slog.Info("identity: loaded users", "count", n)
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
	policySvc := policy.NewService(catalogSnapReader{cat: cat}, selector, limiter)

	// Lifecycle registry — the single point where observer/middleware hooks
	// attach. Hooks register below before pipeline+proxy start serving.
	lifecycleReg := lifecycle.New()

	pl := &pipeline.Pipeline{Policy: policySvc, Lifecycle: lifecycleReg, Logger: slog.Default()}
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
				return plan.HostBinding.Adapter == adapters.OpenAI && plan.Host.Meta.Name == "openai"
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

	// Usage emit: one PostFlight observer that writes JSONL per request.
	// Path defaults to RELAY_USAGE_LOG (or /tmp fallback) — file sink only
	// at this stage; ClickHouse / OTel sinks layer on later by registering
	// alongside.
	usagePath := os.Getenv("RELAY_USAGE_LOG")
	if usagePath == "" {
		usagePath = "relay-usage.jsonl"
	}
	usageSink, err := usagelog.NewFileSink(usagePath)
	if err != nil {
		slog.Error("usagelog: file sink failed; using stdout", "err", err, "path", usagePath)
		usageSink = usagelog.StdoutSink()
	}
	usageEmitter := usagelog.NewEmitter(usagelog.EmitterOptions{}, usageSink)
	defer usageEmitter.Close()
	lifecycleReg.RegisterHook(usagelog.NewUsageHook())
	lifecycleReg.RegisterCollector(usagelog.NewSinkCollector(usageEmitter))
	lifecycleReg.RegisterStreamObserver(usagelog.NewStreamUsageFactory())
	slog.Info("usagelog: wired", "path", usagePath,
		"hooks", lifecycleReg.HookCount(), "collectors", lifecycleReg.CollectorCount(),
		"stream_observers", lifecycleReg.StreamObserverCount())

	// Inference plane (data plane): /v1/*, /healthz on RELAY_PORT.
	inferRouter := chi.NewRouter()
	inferRouter.Use(reqid.Middleware(slog.Default()))
	inference.Mount(inferRouter, inference.Deps{
		Pinger:        st,
		Catalog:       cat,
		Resolver:      routing.New(cat),
		Pipeline:      pl,
		Proxy:         proxyPipeline,
		Lifecycle:     lifecycleReg,
		Adapters:      specRegistry.AdapterMap(),
		Specs:         specRegistry,
		RouteMounters: []inference.RouteMounter{inference.MountRegistry(specRegistry)},
	})

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
			Identity:     idStore,
			Sessions:     sessMgr,
			AdminToken:   cfg.AdminToken,
			Authz:        authz.AlwaysAllowAuthenticated{},
			Catalog:      cat,
			Stores:       stores,
			CookieSecure: cookieSecure,
			UsageReader:  usagelog.NewFileReader(usagePath),
		})
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
