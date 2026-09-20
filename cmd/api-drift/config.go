package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Registries struct {
		ModelsDev  string `yaml:"modelsdev"`
		OpenRouter string `yaml:"openrouter"`
	} `yaml:"registries"`
	Hosts []HostCfg `yaml:"hosts"`
}

type HostCfg struct {
	Name             string   `yaml:"name"`
	Shape            string   `yaml:"shape"` // openai | anthropic
	BaseURL          string   `yaml:"baseURL"`
	KeyEnv           string   `yaml:"keyEnv"`
	RegistryProvider string   `yaml:"registryProvider"` // models.dev top-level key
	ORPrefix         string   `yaml:"orPrefix"`         // OpenRouter model-id prefix
	Models           []string `yaml:"models"`
	Params           []string `yaml:"params"`
}

// loadConfig reads the probe matrix and rejects a host that can't be probed
// (missing credential env, unknown wire shape) before any request goes out.
func loadConfig(path string) Config {
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fatal(err)
	}
	for _, h := range cfg.Hosts {
		if os.Getenv(h.KeyEnv) == "" {
			fatal(fmt.Errorf("host %s: env %s is empty", h.Name, h.KeyEnv))
		}
		if h.Shape != "openai" && h.Shape != "anthropic" {
			fatal(fmt.Errorf("host %s: unknown shape %q", h.Name, h.Shape))
		}
	}
	return cfg
}
