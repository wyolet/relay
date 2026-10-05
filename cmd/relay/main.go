// Command relay is the wyolet-relay data + control plane binary.
//
// New-arch entrypoint: boots app/catalog, mounts the two HTTP planes from
// app/httpapi (inference + control) on separate listeners. Legacy wiring
// against internal/catalog has been moved aside under _legacy/ and will be
// deleted as routes/handlers are ported over.

// Command relay runs the relay server (data and control planes) and its operator subcommands.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settingswatch"
	"github.com/wyolet/relay/internal/config"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkganthropic "github.com/wyolet/relay/sdk/adapters/anthropic"
	pkggemini "github.com/wyolet/relay/sdk/adapters/gemini"
	pkgopenai "github.com/wyolet/relay/sdk/adapters/openai"
	relayv1 "github.com/wyolet/relay/sdk/v1"
)

func main() {
	loadDotEnv(".env")
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

	if runSubcommand() {
		return
	}

	cfg := loadConfig()

	bootCtx := context.Background()

	st := openStorage(bootCtx, cfg)
	defer st.Close()

	bootOpts := catalogBootOptions(cfg, st)
	cat, stores := bootstrapCatalogStores(bootCtx, bootOpts)
	seedSettings(bootCtx, cfg, stores)
	licenseSvc := loadLicense(bootCtx, stores)

	listenerCtx, cancelListener := context.WithCancel(bootCtx)
	defer cancelListener()
	// hydrateLoop launches below, after settings-change subscribers are
	// registered — its first Hydrate runs settings.reload, which notifies
	// subscribers with the stored values. Registering after it would race
	// that one-shot boot notification.

	idStore, usersStore := loadUsers(bootCtx, cfg, st)

	cat.UseTokenVersions(usersStore)

	tokenSigner, tokenVerifier := buildTokenSigning(bootCtx, cfg, st, stores)
	// PUT /license writes the section; the watcher is what carries the change
	// to the other pods (and back to this one after a NOTIFY).
	settingswatch.New(cat, settings.SectionLicense, applyLicenseSection(licenseSvc), slog.Default()).Start()

	watchAuthTokens(listenerCtx, cfg, st, stores, cat, tokenSigner, tokenVerifier)

	seedBuiltinRoles(bootCtx, st, stores)

	kvStore := openKV(bootCtx, cfg)
	defer kvStore.Close()

	startOAuthRefresher(listenerCtx, st, stores, kvStore)

	cookieSecure := os.Getenv("RELAY_COOKIE_SECURE") != "false"
	sessMgr := session.New(kvStore, cookieSecure, "sess:")
	sessMgr.UseGroups(func(userID string) []string { return cat.Current().GroupsForUser(userID) })

	validateOIDCEnv()

	// Pipeline orchestrator: shared limiter + selector backed by kv.
	limiter := pkgratelimit.New(kvStore, slog.Default(), nil)
	selector := keypool.New(kvStore, slog.Default(), nil, nil)
	hostHealth := hosthealth.New(kvStore, nil)
	pl, lifecycleReg := buildPipeline(cat, stores, limiter, selector, hostHealth)
	proxyPipeline := buildProxy(cfg, limiter, lifecycleReg)

	specs := buildAdapterSpecs()
	profiles := buildClientProfiles()
	specRegistry := buildSpecRegistry(specs)

	usageCtl, usageReader := buildUsageLog(listenerCtx, cfg, cat, kvStore, lifecycleReg)
	defer usageCtl.Close()

	// Payload logging: the second lifecycle observer. Always wired; its
	// runtime config lives in the "payload-logging" settings section, so it
	// toggles and reconfigures (backend / bucket / credentials) without a
	// restart. Per-request capture is still gated by the Policy/Key
	// opt-in resolved at the inference entry. S3 credentials resolve through
	// the shared secret registry.
	payloadCHBootCfg := payloadCHBootConfig(cfg)
	payloadCtl := buildPayloadLog(listenerCtx, cat, stores, lifecycleReg, payloadCHBootCfg)
	defer payloadCtl.Close()

	tokenCalibrator := buildTokenCalibrator(kvStore, lifecycleReg)
	registerAdmission(cfg, lifecycleReg)
	registerMetrics(lifecycleReg, usageCtl, payloadCtl)

	// Read side of payload logging: serves the /payloads/* Logs endpoints
	// over whatever backend the live settings name, rebuilt lazily on config
	// change (mirrors the sink Controller).
	payloadReader := newPayloadReaderResolver(cat, stores.Secrets, payloadCHBootCfg, slog.Default())

	auditStore, auditEmitter := buildAudit(st, cat)
	defer auditEmitter.Close()

	watchParsing(cat)

	// All settings-change subscribers are now registered; start background
	// hydration. Its first Hydrate runs settings.reload → notifies them with
	// the stored values (the data plane gates on IsReady until it completes).
	go hydrateLoop(listenerCtx, cat, stores, bootOpts)

	routingOpts := routingOptions(cfg)
	batchQueue, batchSvc := buildBatch(bootCtx, listenerCtx, st, cat, pl, specRegistry, routingOpts, cfg.BatchMaxItems)

	inferSrv, inferErr := startInference(cfg, st, cat, tokenVerifier, routingOpts, pl, proxyPipeline, lifecycleReg,
		specRegistry, profiles, tokenCalibrator, batchSvc, buildOTLPReceiver(cfg, cat, usageCtl, payloadCtl, kvStore, limiter))

	// Control plane (admin plane): /auth/*, CRUD, /version, /reload on
	// RELAY_CONTROL_PORT. Disabled when empty or "off".
	var ctrlSrv *http.Server
	var ctrlErr <-chan error
	if cfg.ControlPort != "" && cfg.ControlPort != "off" {
		ctrlSrv, ctrlErr = startControl(cfg, st, cat, stores, idStore, usersStore, tokenSigner, tokenVerifier,
			kvStore, limiter, sessMgr, licenseSvc, cookieSecure, usageReader, auditEmitter, auditStore, payloadReader,
			selector, hostHealth)
	}

	exitCode = waitForStop(inferErr, ctrlErr)
	shutdown(cfg, ctrlSrv, inferSrv, cancelListener, batchQueue)
}

func buildAdapterSpecs() []*adapter.Spec {
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
			StreamUsage:   &adapter.StreamUsageOptIn{Request: pkgopenai.RequestStreamUsage, IsUsageFrame: pkgopenai.IsUsageOnlyChunk},
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
			CountPath:     "/v1/messages/count_tokens",
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
	return specs
}

func watchParsing(cat *appcatalog.Catalog) {
	// Request-parsing depth lives in the "parsing" settings section and
	// hot-swaps the openai adapter's rich-parse toggle. The vendor setter
	// is confined here (composition root) so app/ stays vendor-neutral.
	settingswatch.New(cat, settings.SectionParsing, func(p settings.Parsing) {
		pkgopenai.SetRichParsing(p.RichParsing)
		slog.Debug("parsing: applied", "rich_parsing", p.RichParsing)
	}, slog.Default()).Start()
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
