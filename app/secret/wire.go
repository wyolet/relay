// Package secret wires relay's secret resolution (the registry over env, stored and external backends) and holds the key agent that heals failed upstream keys out of band. The stored backend itself is the stored subpackage.
package secret

import (
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/secret"
	"github.com/wyolet/relay/secret/backend/aws"
	"github.com/wyolet/relay/secret/backend/azure"
	"github.com/wyolet/relay/secret/backend/bitwarden"
	"github.com/wyolet/relay/secret/backend/gcp"
)

// Secrets is the wired stack: Registry resolves every ref kind, Stored is the stored backend registered in it.
type Secrets struct {
	Registry *secret.Registry
	Stored   *stored.Backend
}

// Wire builds the stack over pool. masterKeys are RELAY_MASTER_KEY followed by RELAY_MASTER_KEY_EXTRA; with none, stored refs fail loudly while env and external refs keep working. External backends (AWS, Azure, GCP, Bitwarden, 1Password) register when their environment is present.
func Wire(pool *pgxpool.Pool, masterKeys [][]byte) (*Secrets, error) {
	backend, err := stored.New(pool, masterKeys)
	if err != nil {
		return nil, err
	}
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	reg.Register(secret.KindStored, backend)

	if cfg, err := aws.ConfigFromEnv(); err == nil {
		reg.Register(secret.KindAWS, aws.New(cfg))
	}
	if cfg, ok := bitwardenConfigFromEnv(); ok {
		reg.Register(secret.KindBitwarden, bitwarden.New(cfg))
	}
	if cfg, err := azure.ConfigFromEnv(); err == nil {
		reg.Register(secret.KindAzure, azure.New(cfg))
	}
	if cfg, err := gcp.ConfigFromEnv(); err == nil {
		reg.Register(secret.KindGCP, gcp.New(cfg))
	}
	// 1Password's SDK requires CGO, so it's only compiled into cgo builds
	// (see onepassword_cgo.go / onepassword_nocgo.go). Relay's default
	// CGO_ENABLED=0 build registers nothing here.
	registerOnePassword(reg)

	return &Secrets{Registry: reg, Stored: backend}, nil
}

func bitwardenConfigFromEnv() (bitwarden.Config, bool) {
	baseURL := os.Getenv("BW_BASE_URL")
	if baseURL == "" {
		return bitwarden.Config{}, false
	}

	interval := 5 * time.Minute
	if raw := os.Getenv("BW_SYNC_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			interval = d
		}
	}

	return bitwarden.Config{
		BaseURL:            baseURL,
		Email:              os.Getenv("BW_EMAIL"),
		MasterPassword:     os.Getenv("BW_MASTER_PASSWORD"),
		ClientID:           os.Getenv("BW_CLIENT_ID"),
		ClientSecret:       os.Getenv("BW_CLIENT_SECRET"),
		SyncInterval:       interval,
		InsecureSkipVerify: os.Getenv("BW_INSECURE_SKIP_TLS_VERIFY") == "1",
		DeviceName:         "relay-bitwarden",
	}, true
}
