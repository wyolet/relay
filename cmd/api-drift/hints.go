package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// hintSet holds the registry witnesses: corroborating evidence only, never
// enough on its own to call drift.
type hintSet struct {
	mdTemp map[string]*bool    // "provider/model" → models.dev temperature flag
	orSupp map[string][]string // OpenRouter id → supported_parameters
}

func fetchHints(cfg Config) *hintSet {
	hs := &hintSet{mdTemp: map[string]*bool{}, orSupp: map[string][]string{}}
	if u := cfg.Registries.ModelsDev; u != "" {
		var doc map[string]struct {
			Models map[string]struct {
				Temperature *bool `json:"temperature"`
			} `json:"models"`
		}
		if err := getJSON(u, &doc); err != nil {
			fmt.Fprintln(os.Stderr, "warn: models.dev fetch:", err)
		} else {
			for prov, p := range doc {
				for id, m := range p.Models {
					hs.mdTemp[prov+"/"+id] = m.Temperature
				}
			}
		}
	}
	if u := cfg.Registries.OpenRouter; u != "" {
		var doc struct {
			Data []struct {
				ID                  string   `json:"id"`
				SupportedParameters []string `json:"supported_parameters"`
			} `json:"data"`
		}
		if err := getJSON(u, &doc); err != nil {
			fmt.Fprintln(os.Stderr, "warn: openrouter fetch:", err)
		} else {
			for _, m := range doc.Data {
				hs.orSupp[m.ID] = m.SupportedParameters
			}
		}
	}
	return hs
}

func (hs *hintSet) for_(h HostCfg, model, param string) map[string]string {
	out := map[string]string{}
	if param == "temperature" && h.RegistryProvider != "" {
		if t, ok := hs.mdTemp[h.RegistryProvider+"/"+model]; ok && t != nil {
			out["models.dev"] = map[bool]string{true: "supported", false: "unsupported"}[*t]
		} else {
			out["models.dev"] = "unknown"
		}
	}
	if h.ORPrefix != "" {
		if supp, ok := hs.orSupp[h.ORPrefix+"/"+model]; ok {
			out["openrouter"] = "unsupported"
			for _, s := range supp {
				if s == param {
					out["openrouter"] = "supported"
				}
			}
		} else {
			out["openrouter"] = "unknown"
		}
	}
	return out
}

func getJSON(url string, v any) error {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
