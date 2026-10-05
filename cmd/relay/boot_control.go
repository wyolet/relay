package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/app/user"
	relayweb "github.com/wyolet/relay/cmd/relay/web"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/identity"
	"github.com/wyolet/relay/internal/license"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func startControl(cfg *config.Config, st *storagemod.Storage, cat *appcatalog.Catalog, stores *appcatalog.Stores,
	idStore *identity.Store, usersStore *user.Store, tokenSigner *control.TokenSigner, tokenVerifier *inference.TokenVerifier,
	kvStore kv.Store, limiter *pkgratelimit.Limiter, sessMgr *session.Manager, licenseSvc *license.Service, cookieSecure bool,
	usageReader usagelog.Reader, auditEmitter *audit.Emitter, auditStore *audit.Store, payloadReader *payloadReaderResolver,
	selector *keypool.Selector, hostHealth *hosthealth.Recorder) (*http.Server, <-chan error) {
	ctrlRouter := chi.NewRouter()
	ctrlRouter.Use(httpmw.SecurityHeaders)
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
	warnSingleUserOpenRegistration(cfg.Authz, settings.EffectiveAuthOIDC(cat))
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
	// /config.json stays at the listener root: the UI fetches it before
	// it knows the /api prefix, which config.json advertises as
	// controlApiUrl.
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
	// The embedded SPA is the fallback for unclaimed paths, but never
	// under /api: a UI calling a renamed endpoint must get a JSON 404 it
	// can parse. Mounted only when a dist was baked in and not disabled.
	if !cfg.UIDisable && relayweb.Present() {
		ctrlRouter.NotFound(relayweb.Handler(cfg.Runtime.ControlAPIURL, cfg.Runtime.InferenceAPIURL, cfg.Runtime.SentryDSN).ServeHTTP)
		slog.Debug("relay control: serving embedded UI")
	}
	ctrlSrv := &http.Server{
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
	go func() { ch <- ctrlSrv.ListenAndServe() }()
	return ctrlSrv, ch
}
