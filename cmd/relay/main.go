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
	apianthropic "github.com/wyolet/relay/app/api/anthropic"
	apiopenai "github.com/wyolet/relay/app/api/openai"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/identity"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func main() {
	loadDotEnv(".env")
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			slog.Info("relay: 'migrate' subcommand currently runs implicitly on boot")
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
	if cfg.AutoSeedIfEmpty {
		bootOpts.AutoSeedDir = cfg.ConfigDir
	}

	cat, listener, stores, err := appcatalog.Bootstrap(bootCtx, bootOpts)
	if err != nil {
		slog.Error("catalog bootstrap failed", "err", err)
		os.Exit(1)
	}

	listenerCtx, cancelListener := context.WithCancel(bootCtx)
	defer cancelListener()
	go func() {
		if err := listener.Run(listenerCtx); err != nil && err != context.Canceled {
			slog.Error("catalog listener exited", "err", err)
		}
	}()
	slog.Info("catalog bootstrapped", "auto_seed_dir", bootOpts.AutoSeedDir)

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
	pl := &pipeline.Pipeline{
		Limiter:  limiter,
		Selector: selector,
		Logger:   slog.Default(),
	}

	// Adapter registry — one entry per supported wire protocol.
	adapters := map[adapter.Kind]pipeline.Adapter{
		adapter.OpenAI:    apiopenai.New(),
		adapter.Anthropic: apianthropic.New(),
	}

	// Inference plane (data plane): /v1/*, /healthz on RELAY_PORT.
	inferRouter := chi.NewRouter()
	inference.Mount(inferRouter, inference.Deps{
		Pinger:   st,
		Catalog:  cat,
		Resolver: routing.New(cat),
		Pipeline: pl,
		Adapters: adapters,
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
