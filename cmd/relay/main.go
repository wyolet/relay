package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"

	apiopenai "github.com/wyolet/relay/pkg/api/openai"
	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/keypool"
	"github.com/wyolet/relay/pkg/pipeline"
	"github.com/wyolet/relay/pkg/provider"
	"github.com/wyolet/relay/pkg/provider/ollama"
	"github.com/wyolet/relay/pkg/provider/openai"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/state"
	"github.com/wyolet/relay/pkg/transport"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	var cfg configstore.ConfigStore
	yamlStore, err := configstore.LoadYAML("config")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg = yamlStore

	// In-memory state store for key circuit breakers.
	st := state.New()
	sel := keypool.New(st, slog.Default(), nil)

	// Build provider clients and register them.
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

		// Resolve pool and secrets for the provider's default pool.
		if poolName := p.Spec.DefaultPool; poolName != "" {
			if pool, ok := cfg.PoolByName(poolName); ok {
				plan.Pool = pool
				plan.Secrets = cfg.SecretsForPool(pool)
			}
		}
		return plan, true
	}

	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *apiopenai.RequestPlan) error {
		ob, err := reg.Get(plan.Provider.Spec.Kind)
		if err != nil {
			return err
		}

		// If we have a pool with keys, use the orchestrating Run.
		if plan.Pool != nil && len(plan.Secrets) > 0 {
			return pipeline.Run(ctx, ch, pipeline.RunOptions{
				Pool:     plan.Pool,
				Secrets:  plan.Secrets,
				Selector: sel,
				Outbound: ob,
			})
		}

		// Fallback for providers without a pool (e.g., anonymous Ollama).
		// Use a synthetic pool with an empty-secret key.
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

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.MaxRequestBytesFromEnv()))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	r.Post("/v1/chat/completions", apiopenai.ChatCompletions(resolve, runPipeline))
	r.Get("/v1/models", apiopenai.ListModels(cfg))

	addr := ":8080"
	log.Printf("relay listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}
