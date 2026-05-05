package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/pkg/api/openai"
	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/pipeline"
	"github.com/wyolet/relay/pkg/provider/ollama"
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

	p := cfg.DefaultProvider()
	if p == nil {
		log.Fatal("config: no default provider")
	}
	if p.Spec.Kind != configstore.PKOllama {
		log.Fatalf("config: only ollama supported for now, got %q", p.Spec.Kind)
	}

	client := ollama.New(p.Spec.BaseURL)

	resolve := func(name string) (string, bool) {
		m, ok := cfg.ModelByName(name)
		if !ok {
			return "", false
		}
		return m.Spec.UpstreamName, true
	}

	ollamaOutbound := func(ctx context.Context, body []byte, out chan<- *transport.Message) error {
		return client.ChatCompletions(ctx, body, out)
	}

	runPipeline := func(ctx context.Context, ch *transport.Channel) error {
		return pipeline.Run(ctx, ch, ollamaOutbound)
	}

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.MaxRequestBytesFromEnv()))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	r.Post("/v1/chat/completions", openai.ChatCompletions(resolve, runPipeline))
	r.Get("/v1/models", openai.ListModels(cfg))

	addr := ":8080"
	log.Printf("relay listening on %s; default provider=%s (%s)", addr, p.Metadata.Name, p.Spec.BaseURL)
	log.Fatal(http.ListenAndServe(addr, r))
}
