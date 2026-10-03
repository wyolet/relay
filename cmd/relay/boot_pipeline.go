package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/wyolet/relay/app/adapter"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func buildPipeline(cat *appcatalog.Catalog, stores *appcatalog.Stores, limiter *pkgratelimit.Limiter, selector *keypool.Selector,
	hostHealth *hosthealth.Recorder) (*pipeline.Pipeline, *lifecycle.Registry) {
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
	return pl, lifecycleReg
}

func buildProxy(cfg *config.Config, limiter *pkgratelimit.Limiter, lifecycleReg *lifecycle.Registry) *proxy.Pipeline {
	proxyPipeline := proxy.New(limiter, lifecycleReg, slog.Default())

	// Upstream connection pooling: applies to every adapter Spec built below
	// and to the proxy runner's client. Must run before the specs.
	adapter.SetUpstreamMaxIdleConnsPerHost(cfg.UpstreamMaxIdlePerHost)
	adapter.SetUpstreamStreamIdleTimeout(cfg.StreamIdleTimeout)
	proxyPipeline.Client = &http.Client{Transport: adapter.NewUpstreamTransport(false)}
	return proxyPipeline
}

func buildClientProfiles() *clientprofile.Registry {
	profiles := clientprofile.New()
	for _, p := range []clientprofile.Profile{clientprofile.ClaudeCode(), clientprofile.Codex(), clientprofile.OpenCode()} {
		if err := profiles.Register(p); err != nil {
			slog.Error("client profile registration failed", "err", err)
			os.Exit(1)
		}
	}
	return profiles
}

func buildSpecRegistry(specs []*adapter.Spec) *adapter.Registry {
	specRegistry := adapter.NewRegistry(specs...)
	if err := specRegistry.AssertWired(); err != nil {
		slog.Error("adapter registry mis-wired", "err", err)
		os.Exit(1)
	}
	return specRegistry
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
