package main

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/pkg/api/openai"
	"github.com/wyolet/relay/pkg/config"
	"github.com/wyolet/relay/pkg/provider/ollama"
)

func main() {
	cfg, err := config.Load("config")
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	p := cfg.DefaultProvider()
	if p == nil {
		log.Fatal("config: no default provider")
	}
	if p.Spec.Kind != config.PKOllama {
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

	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	r.Post("/v1/chat/completions", openai.ChatCompletions(resolve, client.ChatCompletions))

	addr := ":8080"
	log.Printf("relay listening on %s; default provider=%s (%s)", addr, p.Metadata.Name, p.Spec.BaseURL)
	log.Fatal(http.ListenAndServe(addr, r))
}
