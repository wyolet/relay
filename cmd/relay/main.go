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
	"errors"
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
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/batch"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/keypool"
	applicense "github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/metricslog"
	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/routing"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settingswatch"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/app/user"
	relayweb "github.com/wyolet/relay/cmd/relay/web"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/identity"
	"github.com/wyolet/relay/internal/license"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/jobq"
	"github.com/wyolet/relay/jobq/payload"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/reqid"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
	secretoauth "github.com/wyolet/relay/pkg/secret/oauth"
	pkganthropic "github.com/wyolet/relay/sdk/adapters/anthropic"
	pkggemini "github.com/wyolet/relay/sdk/adapters/gemini"
	pkgopenai "github.com/wyolet/relay/sdk/adapters/openai"
	relayv1 "github.com/wyolet/relay/sdk/v1"
)

// builtinRoleSeedLock is the advisory-lock id the built-in role seed
// serializes on. Arbitrary but fixed: every pod must pick the same number.
const builtinRoleSeedLock int64 = 0x52454C41595F5242

func main() {
	loadDotEnv(".env")
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			if err := runMigrate(os.Args[2:]); err != nil {
				slog.Error("migrate failed", "err", err)
				os.Exit(1)
			}
			return
		case "seed":
			if err := runSeed(os.Args[2:]); err != nil {
				slog.Error("seed failed", "err", err)
				os.Exit(1)
			}
			return
		case "apply":
			runCLI("apply", runApply, os.Args[2:])
			return
		case "export":
			runCLI("export", runExport, os.Args[2:])
			return
		case "keygen":
			runCLI("keygen", runKeygen, os.Args[2:])
			return
		case "token":
			runCLI("token", runToken, os.Args[2:])
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

	if !cfg.MigrateOnBoot {
		slog.Warn("storage: boot migrations disabled (RELAY_MIGRATE_ON_BOOT=off); the schema is left as found")
	}
	st, err := storagemod.Open(bootCtx, cfg.PGDSN,
		storagemod.WithMaxConns(cfg.PGMaxConns),
		storagemod.WithMinConns(cfg.PGMinConns),
		storagemod.WithMigrateOnBoot(cfg.MigrateOnBoot))
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
	if cfg.CatalogVersion != "" {
		bootOpts.CatalogVersion = cfg.CatalogVersion
		bootOpts.CatalogURL = cfg.CatalogURL
		bootOpts.CatalogIndexURL = cfg.CatalogIndexURL
		slog.Info("catalog: version pinned", "version", cfg.CatalogVersion)
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

	// License: verified offline, never fatal. Resolved before hydrate so
	// the first settings decode already sees the gate. The environment wins
	// over the stored value; a bad or expired one degrades to community.
	licenseSvc := license.New(nil)
	var storedLicense string
	if row, err := stores.Settings.Get(bootCtx, settings.SectionLicense); err == nil {
		if l, ok := row.Value.(*settings.License); ok {
			storedLicense = l.Value
		}
	}
	if info, err := licenseSvc.Set(storedLicense); err != nil {
		slog.Warn("license: unusable — running as community", "err", err)
	} else if info.Licensed {
		slog.Info("license: verified", "customer", info.Customer,
			"expiresAt", info.ExpiresAt, "features", info.Features, "grace", info.Grace)
	}
	settings.SetLicenseGate(licenseSvc)

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

	// DB-backed users: login reads the table; YAML identity is the
	// seed-if-absent bootstrap (and break-glass fallback at login).
	usersStore := user.NewStore(gen.New(st.Pool()))
	if err := user.SeedFromIdentity(bootCtx, usersStore, idStore, slog.Default()); err != nil {
		slog.Error("user seed from identity YAML failed", "err", err)
		os.Exit(1)
	}

	cat.UseTokenVersions(usersStore)

	// Inference tokens: the signing key is generated on first boot and kept
	// under the master key; both planes hold it in memory and follow the
	// auth:tokens section from there.
	tokenSigner := &control.TokenSigner{}
	tokenVerifier := &inference.TokenVerifier{}
	if err := loadTokenSigningKey(bootCtx, st.Pool(), stores, cfg.MasterKey, tokenSigner, tokenVerifier); err != nil {
		slog.Error("auth: inference-token signing key unavailable", "err", err)
		os.Exit(1)
	}
	// PUT /license writes the section; the watcher is what carries the change
	// to the other pods (and back to this one after a NOTIFY).
	settingswatch.New(cat, settings.SectionLicense, applyLicenseSection(licenseSvc), slog.Default()).Start()

	settingswatch.New(cat, settings.AuthTokensSection, func(a settings.AuthTokens) {
		if err := applyAuthTokensSection(listenerCtx, st.Pool(), stores, cfg.MasterKey, a, tokenSigner, tokenVerifier); err != nil {
			slog.Error("auth: inference-token signing key reload failed", "err", err)
		}
	}, slog.Default()).Start()

	// Built-in roles: seed-if-absent, so an operator's edits survive and a
	// fresh deployment always has the seven system rows to bind against.
	seedLock := func(ctx context.Context, fn func(context.Context) error) error {
		return storagemod.WithAdvisoryLock(ctx, st.Pool(), builtinRoleSeedLock, fn)
	}
	if err := role.SeedBuiltins(bootCtx, stores.Role, slog.Default(), seedLock); err != nil {
		slog.Error("built-in role seed failed", "err", err)
		os.Exit(1)
	}

	// kv backend — sessions, rate-limits, key-pool all share this.
	var kvStore kv.Store
	if cfg.StateBackend == "redis" {
		if cfg.RedisAddr == "" {
			slog.Error("RELAY_REDIS_ADDR required when RELAY_STATE_BACKEND=redis")
			os.Exit(1)
		}
		rs, err := kv.NewRedis(bootCtx, kv.RedisConfig{
			Addr:         cfg.RedisAddr,
			PoolSize:     cfg.RedisPoolSize,
			MinIdleConns: cfg.RedisMinIdleConns,
		})
		if err != nil {
			slog.Error("state(redis) init failed", "err", err)
			os.Exit(1)
		}
		kvStore = rs
	} else {
		kvStore = kv.NewMem()
	}
	defer kvStore.Close()

	// Proactive OAuth renewal: keeps subscription tokens fresh ahead of
	// expiry so requests never pay refresh latency. Cluster-safe via the kv
	// lock; outcomes persist on the hostkey status (UI-visible on the same
	// key) and broadcast through the catalog hostkey NOTIFY so every pod
	// reloads the credential (secret_values has no trigger of its own).
	oauthRefs := func(ctx context.Context) ([]pkgsecret.Ref, error) {
		keys, err := stores.HostKey.List(ctx)
		if err != nil {
			return nil, err
		}
		var refs []pkgsecret.Ref
		for _, k := range keys {
			if k.Spec.ValueFrom.Kind != hostkey.ValueKindOAuth {
				continue
			}
			// Revoked grants wait for operator re-auth (a value update
			// clears the status) — the refresher contract excludes them.
			if c := k.Status.Credential; c != nil && c.State == hostkey.CredentialRevoked {
				continue
			}
			refs = append(refs, pkgsecret.Ref{
				Kind: pkgsecret.KindOAuth, ID: k.Meta.ID, Provider: k.Spec.ValueFrom.Provider,
			})
		}
		return refs, nil
	}
	oauthNotify := func(ctx context.Context, id string) error {
		_, err := st.Pool().Exec(ctx, "select pg_notify('catalog_events', $1)", "hostkey:upsert:"+id)
		return err
	}
	oauthHooks := secretoauth.Hooks{
		OnRenewed: func(ctx context.Context, id string, expiresAt time.Time) error {
			now := time.Now().UTC()
			if err := stores.HostKey.SetCredentialStatus(ctx, id, hostkey.CredentialStatus{
				State: hostkey.CredentialOK, ExpiresAt: expiresAt, RenewedAt: now, At: now,
			}); err != nil {
				return err
			}
			return oauthNotify(ctx, id)
		},
		OnRevoked: func(ctx context.Context, id string, cause error) error {
			if err := stores.HostKey.SetCredentialStatus(ctx, id, hostkey.CredentialStatus{
				State: hostkey.CredentialRevoked, LastError: cause.Error(), At: time.Now().UTC(),
			}); err != nil {
				return err
			}
			return oauthNotify(ctx, id)
		},
	}
	go secretoauth.NewRefresher(oauthRefs, stores.OAuthResolver, kvStore, oauthHooks, slog.Default()).
		Run(listenerCtx)

	cookieSecure := os.Getenv("RELAY_COOKIE_SECURE") != "false"
	sessMgr := session.New(kvStore, cookieSecure, "sess:")
	sessMgr.UseGroups(func(userID string) []string { return cat.Current().GroupsForUser(userID) })

	// WYOLET_* OIDC env overlay: validate at boot so a typo'd overlay fails
	// the boot, not the first login attempt.
	// An unlicensed overlay is refused, not fatal: a deployment that loses
	// its license keeps booting and keeps serving password login.
	if oidcEnv, err := settings.AuthOIDCEnv(); errors.Is(err, applicense.ErrRequired) {
		slog.Warn("auth: oidc login requires a license — falling back to password login", "err", err)
	} else if err != nil {
		slog.Error("auth: invalid WYOLET_* OIDC env overlay", "err", err)
		os.Exit(1)
	} else if oidcEnv != nil {
		slog.Info("auth: oidc login enabled via WYOLET_AUTH_MODE",
			"issuer", oidcEnv.Issuer, "registration", oidcEnv.Registration)
	} else if mode := os.Getenv("WYOLET_AUTH_MODE"); mode != "" && mode != "oidc" {
		slog.Warn("auth: WYOLET_AUTH_MODE not implemented by relay; password login remains the no-IdP path", "mode", mode)
	}

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

	// Upstream connection pooling: applies to every adapter Spec built below
	// and to the proxy runner's client. Must run before the specs.
	adapter.SetUpstreamMaxIdleConnsPerHost(cfg.UpstreamMaxIdlePerHost)
	proxyPipeline.Client = &http.Client{Transport: adapter.NewUpstreamTransport(false)}

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
			DefaultPath:   "/v1/chat/completions",
			Auth:          openaiAuth,
			Translator:    pkgopenai.CCTranslator{},
			ExtractTokens: pkgopenai.ExtractTokens,
			ParamPaths:    map[string]string{"temperature": "temperature", "top_p": "top_p"},
		}).Build(),
		(&adapter.Spec{
			Name: adapters.OpenAIResponses,
			InboundPaths: []adapter.InboundPath{
				{Path: "/openai/v1/responses", OperationID: "openai_responses_create", Summary: "Create a response (OpenAI Responses API)"},
			},
			DefaultPath:   "/v1/responses",
			Auth:          openaiAuth,
			Translator:    pkgopenai.ResponsesTranslator{},
			ExtractTokens: pkgopenai.ExtractTokens,
			ParamPaths:    map[string]string{"temperature": "temperature", "top_p": "top_p"},
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
			DefaultPath:   "/v1/embeddings",
			Auth:          openaiAuth,
			BytePass:      true,
			ExtractTokens: pkgopenai.ExtractTokens,
		}).Build(),
		(&adapter.Spec{
			Name: adapters.Anthropic,
			InboundPaths: []adapter.InboundPath{
				{Path: "/anthropic/v1/messages", OperationID: "anthropic_messages", Summary: "Create a message (Anthropic Messages shape)"},
			},
			DefaultPath:   "/v1/messages",
			Auth:          anthropicAuth,
			Translator:    pkganthropic.AnthropicTranslator{},
			ExtractTokens: pkganthropic.ExtractTokens,
			ParamPaths:    map[string]string{"temperature": "temperature", "top_p": "top_p", "top_k": "top_k"},
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
			ParamPaths: map[string]string{
				"temperature": "generationConfig.temperature",
				"top_p":       "generationConfig.topP",
				"top_k":       "generationConfig.topK",
			},
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

	// Payload logging: the second lifecycle observer. Always wired; its
	// runtime config lives in the "payload-logging" settings section, so it
	// toggles and reconfigures (backend / bucket / credentials) without a
	// restart. Per-request capture is still gated by the Policy/Key
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

	// Read side of payload logging: serves the /payloads/* Logs endpoints
	// over whatever backend the live settings name, rebuilt lazily on config
	// change (mirrors the sink Controller).
	payloadReader := newPayloadReaderResolver(cat, stores.Secrets, payloadCHBootCfg, slog.Default())

	// Admin audit log: bounded emitter → PG, retention from the "audit"
	// settings section. Wired before hydration so the first settings reload
	// lands on the emitter.
	auditStore := audit.NewStore(gen.New(st.Pool()))
	auditEmitter := audit.NewEmitter(auditStore, slog.Default())
	defer auditEmitter.Close()
	settingswatch.New(cat, settings.SectionAudit, func(a settings.Audit) {
		auditEmitter.SetRetentionDays(a.RetentionDays)
	}, slog.Default()).Start()

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
		batchCaller,
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
	maxBody := cfg.MaxRequestBytes
	if maxBody <= 0 {
		maxBody = httpmw.DefaultMaxRequestBytes
	}
	inferRouter.Use(httpmw.LimitBody(maxBody))
	inference.Mount(inferRouter, inference.Deps{
		Pinger:         st,
		Catalog:        cat,
		Tokens:         tokenVerifier,
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
	// key auth), mounted directly on chi like /v1/ws since it isn't a
	// huma operation.
	inferRouter.With(
		inference.ReadinessMiddleware(cat),
		inference.ClassifyMiddleware(),
		inference.PrincipalMiddleware(cat, tokenVerifier),
	).Mount("/v1/batches", batchSvc.Routes())

	inferAddr := ":8080"
	if p := os.Getenv("RELAY_PORT"); p != "" {
		inferAddr = ":" + p
	}
	inferSrv := &http.Server{
		Addr:              inferAddr,
		Handler:           inferRouter,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
		// WriteTimeout stays 0 (unbounded): SSE responses are long-lived streams,
		// and a write deadline is absolute — it would truncate a generation
		// mid-flight. Header/idle limits plus the in-flight admission cap bound
		// resource use instead of a response-duration cap.
	}
	slog.Info("relay inference listening", "addr", inferAddr)
	inferErr := make(chan error, 1)
	go func() { inferErr <- inferSrv.ListenAndServe() }()

	// Control plane (admin plane): /auth/*, CRUD, /version, /reload on
	// RELAY_CONTROL_PORT. Disabled when empty or "off".
	var ctrlSrv *http.Server
	var ctrlErr <-chan error
	if cfg.ControlPort != "" && cfg.ControlPort != "off" {
		ctrlRouter := chi.NewRouter()
		if len(cfg.ControlAllowOrigins) > 0 {
			ctrlRouter.Use(control.CORS(cfg.ControlAllowOrigins...))
		}
		// Control API under /api so its CRUD paths (/models, /policies, …) don't
		// shadow the SPA's identically-named client-side routes on the shared
		// control origin (a hard-reload of /models must serve the UI, not JSON).
		var authorizer authz.Authorizer = authz.AlwaysAllowAuthenticated{}
		if cfg.Authz == config.AuthzRBAC {
			authorizer = authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
		}
		// Which authorizer is live decides whether an authenticated user is
		// an admin; an upgrade that silently picks the wrong one is exactly
		// what an operator needs to see in the first lines of a boot log.
		slog.Info("relay control: authorization mode", "authz", cfg.Authz)
		authorizer = audit.Authorizer{Inner: authorizer, Snap: cat.Current}
		ctrlDeps := control.Deps{
			Identity:      idStore,
			TokenSigner:   tokenSigner,
			TokenDenylist: kvStore,
			MintLimiter:   limiter,
			RotateTokenKey: func(ctx context.Context) error {
				return rotateTokenSigningKey(ctx, st.Pool(), stores, cfg.MasterKey, tokenSigner, tokenVerifier)
			},
			Users:          usersStore,
			Sessions:       sessMgr,
			AdminToken:     cfg.AdminToken,
			Authz:          authorizer,
			License:        licenseSvc,
			Catalog:        cat,
			Stores:         stores,
			CookieSecure:   cookieSecure,
			UsageReader:    usageReader,
			Audit:          auditEmitter,
			AuditReader:    auditStore,
			TrustedProxies: httpmw.TrustedProxies(),
			PayloadReader:  payloadReader,
			Selector:       selector,
			HostHealth:     hostHealth,
			PublicURL:      cfg.PublicURL,
			RuntimeConfig:  runtimeConfig(cfg),
		}
		// /config.json stays at the listener ROOT — the UI fetches it at boot,
		// before it knows the /api prefix. It advertises controlApiUrl=/api so
		// the SPA's API client targets /api/* while the SPA's own routes
		// (/models, /policies, …) fall through to the embedded UI below.
		ctrlRouter.Get("/config.json", control.ConfigJSONHandler(ctrlDeps))
		ctrlRouter.Route("/api", func(r chi.Router) {
			control.Mount(r, ctrlDeps)
		})
		// OIDC callback at the listener ROOT: redirect URIs are registered
		// as <origin>/auth/callback, and a registered URI must match
		// byte-exactly — it can't carry the /api prefix the rest of the
		// control API mounts under.
		control.MountOIDCCallbackRoot(ctrlRouter, ctrlDeps)
		ctrlRouter.Handle("/metrics", metrics.Handler())
		// Embedded admin UI: same-origin SPA served as the fallback for
		// everything the routes above do not claim. Paths under /api never
		// reach it — the control API answers its own 404s in JSON, so a UI
		// calling a renamed endpoint gets an error it can parse. Only
		// mounted when a real dist was baked in (image build) and not
		// explicitly disabled.
		if !cfg.UIDisable && relayweb.Present() {
			ctrlRouter.NotFound(relayweb.Handler().ServeHTTP)
			slog.Debug("relay control: serving embedded UI")
		}
		ctrlSrv = &http.Server{
			Addr:              ":" + cfg.ControlPort,
			Handler:           ctrlRouter,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20, // 1 MiB
			// WriteTimeout stays 0: the control plane serves /metrics scrapes and
			// admin CRUD, but shares the process with the data plane's SSE
			// constraint and gains nothing from a response-duration cap here.
		}
		slog.Info("relay control listening", "addr", ctrlSrv.Addr, "users", len(idStore.Users()))
		ch := make(chan error, 1)
		ctrlErr = ch
		go func() { ch <- ctrlSrv.ListenAndServe() }()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-quit:
		slog.Info("relay: received signal, shutting down", "signal", sig.String())
	case err := <-inferErr:
		if err != nil && err != http.ErrServerClosed {
			exitCode = 1
			slog.Error("relay inference: server error", "err", err)
		}
	case err := <-ctrlErr:
		if err != nil && err != http.ErrServerClosed {
			exitCode = 1
			slog.Error("relay control: server error", "err", err)
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
	// Drain in-flight batch jobs so the graceful-requeue path can run;
	// without this the process exits mid-handler and interrupted jobs sit
	// `running` until the rescuer discards them (MaxAttempts=1).
	batchQueue.Wait()
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

// catalogSnapReader adapts *appcatalog.Catalog to policy.SnapshotReader. It
// serves the snapshot the request was authenticated against, so the rules a
// request is metered by come from the same catalog view its policy did; off
// the request path (batch, boot) there is none and the current one answers.
type catalogSnapReader struct{ cat *appcatalog.Catalog }

func (r catalogSnapReader) snap(ctx context.Context) *appcatalog.Snapshot {
	if s := inference.SnapshotFrom(ctx); s != nil {
		return s
	}
	return r.cat.Current()
}

func (r catalogSnapReader) Policy(ctx context.Context, id string) (*policy.Policy, bool) {
	return r.snap(ctx).Policy(id)
}

func (r catalogSnapReader) RateLimit(ctx context.Context, id string) (*ratelimit.RateLimit, bool) {
	return r.snap(ctx).RateLimit(id)
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
	// The control API is mounted under /api so its CRUD paths don't shadow the
	// embedded SPA's client-side routes on the shared control origin. Advertise
	// that prefix to the UI by default; an explicit RELAY_CONTROL_API_URL wins.
	if rc.ControlAPIURL == "" {
		rc.ControlAPIURL = "/api"
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
