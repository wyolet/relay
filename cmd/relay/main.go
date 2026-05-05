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
	"github.com/wyolet/relay/pkg/pipeline"
	"github.com/wyolet/relay/pkg/provider"
	"github.com/wyolet/relay/pkg/provider/ollama"
	"github.com/wyolet/relay/pkg/provider/openai"
	"github.com/wyolet/relay/pkg/reqid"
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

	// Build provider clients and register them.
	reg := provider.NewRegistry()

	// Ollama: find provider by kind and register.
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
		return &apiopenai.RequestPlan{Model: m, Provider: p}, true
	}

	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *apiopenai.RequestPlan) error {
		ob, err := reg.Get(plan.Provider.Spec.Kind)
		if err != nil {
			return err
		}

		// Secret selection: placeholder for PER-227.
		// Ollama: anonymous (empty secret).
		// OpenAI: first secret from the provider's default pool.
		secret := ""
		if plan.Provider.Spec.Kind != configstore.PKOllama {
			if poolName := plan.Provider.Spec.DefaultPool; poolName != "" {
				if pool, ok := cfg.PoolByName(poolName); ok {
					secrets := cfg.SecretsForPool(pool)
					if len(secrets) > 0 {
						secret = secrets[0].Resolved
					}
				}
			}
		}

		bound := func(ctx context.Context, body []byte, out chan<- *transport.Message) error {
			return ob.ChatCompletions(ctx, body, secret, out)
		}
		return pipeline.Run(ctx, ch, bound)
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
