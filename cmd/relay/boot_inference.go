package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/batch"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/tokencount"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/reqid"
)

func startInference(cfg *config.Config, st *storagemod.Storage, cat *appcatalog.Catalog, tokenVerifier *inference.TokenVerifier,
	routingOpts []routing.Option, pl *pipeline.Pipeline, proxyPipeline *proxy.Pipeline, lifecycleReg *lifecycle.Registry,
	specRegistry *adapter.Registry, profiles *clientprofile.Registry, tokenCalibrator *tokencount.Calibrator,
	batchSvc *batch.Service) (*http.Server, <-chan error) {
	// Inference plane (data plane): /v1/*, /healthz on RELAY_PORT.
	inferRouter := chi.NewRouter()
	inferRouter.Use(reqid.Middleware(slog.Default()))
	maxBody := cfg.MaxRequestBytes
	if maxBody <= 0 {
		maxBody = httpmw.DefaultMaxRequestBytes
	}
	inferRouter.Use(httpmw.LimitBody(maxBody))
	inference.Mount(inferRouter, inference.Deps{
		Pinger:          st,
		Catalog:         cat,
		Tokens:          tokenVerifier,
		Resolver:        routing.New(cat, routingOpts...),
		Pipeline:        pl,
		Proxy:           proxyPipeline,
		Lifecycle:       lifecycleReg,
		Adapters:        specRegistry.AdapterMap(),
		Specs:           specRegistry,
		Profiles:        profiles,
		RouteMounters:   []inference.RouteMounter{inference.MountRegistry(specRegistry)},
		PublicURL:       cfg.Runtime.InferenceAPIURL,
		TokenCalibrator: tokenCalibrator,
		StreamKeepAlive: cfg.StreamKeepAlive,
		TrustEventTime:  cfg.DevTrustEventTime,
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
	return inferSrv, inferErr
}
