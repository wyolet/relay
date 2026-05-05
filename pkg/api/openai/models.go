package openai

import (
	"encoding/json"
	"net/http"

	"github.com/wyolet/relay/pkg/configstore"
)

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// ListModels returns an http.HandlerFunc serving GET /v1/models.
// Reads the catalog from the supplied ConfigStore and emits the
// OpenAI list response shape. created is 0 — a stable placeholder
// (could be config load time later; zero is fine).
func ListModels(cfg configstore.ConfigStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		models := cfg.Models()
		data := make([]modelObject, 0, len(models))
		for _, m := range models {
			data = append(data, modelObject{
				ID:      m.Metadata.Name,
				Object:  "model",
				Created: 0,
				OwnedBy: m.Spec.Provider,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(modelList{Object: "list", Data: data})
	}
}
