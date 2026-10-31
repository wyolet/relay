package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/wyolet/relay/app/adapter"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/proxy"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// postFlightWriters write after the response, on the post-flight kv store. They hold no state the request path's limiter and selector need, so separate instances are safe.
type postFlightWriters struct {
	limiter    *pkgratelimit.Limiter
	selector   *keypool.Selector
	hostHealth *hosthealth.Recorder
}

func newPostFlightWriters(s kv.Store) postFlightWriters {
	return postFlightWriters{
		limiter:    pkgratelimit.New(s, slog.Default(), nil),
		selector:   keypool.New(s, slog.Default(), nil, nil),
		hostHealth: hosthealth.New(s, nil),
	}
}

// buildLimiter counts the plans' counters on limiter, settling on the post-flight store.
func buildLimiter(limiter *pkgratelimit.Limiter, postFlight postFlightWriters) limits.Limiter {
	return lua.New(limiter).WithPostFlight(postFlight.limiter)
}

// buildPipeline wires the realtime pipeline.
func buildPipeline(lim limits.Limiter, selector *keypool.Selector, postFlight postFlightWriters, cat *appcatalog.Catalog, credentials *hostkey.Store, plans *planEngine) (*pipeline.Pipeline, *lifecycle.Registry) {
	// Lifecycle registry — the single point where observer/middleware hooks
	// attach. Hooks register below before pipeline+proxy start serving.
	lifecycleReg := lifecycle.New()

	pl := &pipeline.Pipeline{
		Limiter:      lim,
		Selector:     selector,
		PostSelector: postFlight.selector,
		Lifecycle:    lifecycleReg,
		Logger:       slog.Default(),
		// On an upstream auth failure the agent re-resolves the key's secret
		// out-of-band (rotation), failing over without blocking when other
		// candidates exist and parking only when this key is the last resort.
		KeyAgent:   appsecret.NewAgent(keyRefresher{store: credentials, plans: plans.engine, cat: cat}, 0, slog.Default()),
		HostHealth: postFlight.hostHealth,
	}
	return pl, lifecycleReg
}

func buildProxy(cfg *config.Config, lim limits.Limiter, lifecycleReg *lifecycle.Registry) *proxy.Pipeline {
	proxyPipeline := proxy.New(lim, lifecycleReg, slog.Default())

	// Upstream connection pooling: applies to every adapter Spec built below
	// and to the proxy runner's client. Must run before the specs.
	adapter.SetUpstreamMaxIdleConnsPerHost(upstreamMaxIdlePerHost(cfg))
	adapter.SetUpstreamDialTimeout(cfg.UpstreamDialTimeout)
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

// upstreamMaxIdlePerHost defaults to the in-flight cap: each request in flight holds at most one upstream connection, and an idle cap below that peak closes connections the next requests must dial again.
func upstreamMaxIdlePerHost(cfg *config.Config) int {
	if cfg.UpstreamMaxIdlePerHost > 0 {
		return cfg.UpstreamMaxIdlePerHost
	}
	if cfg.MaxInflight > 0 {
		return cfg.MaxInflight
	}
	return httpapi.DefaultMaxInflight
}

// keyRefresher re-resolves a credential's secret after an upstream auth failure and heals the plans with it, so a rotated key recovers without a restart.
type keyRefresher struct {
	store *hostkey.Store
	plans *engine.Engine
	// cat keeps the control plane's copy of the credential in step; a refresh writes no row, so no notification carries it.
	cat *appcatalog.Catalog
}

// Refresh fetches the credential, applies it when its value moved, and returns the value the published plans now hold: the engine's view, not the request path's root, since the root is swapped by a hook Apply may return before.
func (r keyRefresher) Refresh(ctx context.Context, keyID string) (string, bool, error) {
	k, err := r.store.Get(ctx, keyID)
	if err != nil || k == nil {
		return "", false, err
	}
	cur := engine.Get[plan.KeySlot](r.plans.View(), keyID)
	changed := cur == nil || cur.Credential.Resolved != k.Resolved
	if changed {
		if err := r.cat.ApplyHostKeyUpsert(k); err != nil {
			slog.Warn("key refresh: control-plane copy not updated", "key_id", keyID, "err", err)
		}
		if err := engine.Apply(r.plans, k); err != nil {
			return k.Resolved, true, err
		}
		if healed := engine.Get[plan.KeySlot](r.plans.View(), keyID); healed != nil {
			return healed.Credential.Resolved, true, nil
		}
	}
	return k.Resolved, changed, nil
}
