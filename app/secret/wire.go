package secret

import (
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
	"github.com/wyolet/relay/pkg/secret/aws"
	"github.com/wyolet/relay/pkg/secret/bitwarden"
	"github.com/wyolet/relay/pkg/secret/onepassword"
)

// Wire builds the relay's secret-resolution stack over Postgres: the
// secret_values store, a StoredResolver (AES-GCM, master-key holder), and a
// Registry wiring the built-in env + stored backends. The returned
// StoredResolver is the rotation/version authority; the Registry is the
// resolve entry point. Both share one StoredResolver instance, so a rotation
// swaps the live key for every consumer.
//
// masterKey may be nil for env-only deployments — stored resolution then
// errors loudly, which is the intended behavior when no key is configured.
// Optional external backends (Bitwarden, AWS SM, …) register here when their
// env config is present.
func Wire(q *gen.Queries, pool *pgxpool.Pool, masterKey []byte) (*pkgsecret.Registry, *pkgsecret.StoredResolver) {
	store := NewStore(q, pool)
	stored := pkgsecret.NewStoredResolver(store, masterKey, 1)

	reg := pkgsecret.NewRegistry()
	reg.Register(pkgsecret.KindEnv, pkgsecret.EnvResolver{})
	reg.Register(pkgsecret.KindStored, stored)

	if cfg, err := aws.ConfigFromEnv(); err == nil {
		reg.Register(pkgsecret.KindAWS, aws.New(cfg))
	}
	if cfg, ok := bitwardenConfigFromEnv(); ok {
		reg.Register(pkgsecret.KindBitwarden, bitwarden.New(cfg))
	}
	if cfg, ok := onepassword.ConfigFromEnv(); ok {
		reg.Register(pkgsecret.KindOnePassword, onepassword.New(cfg))
	}

	return reg, stored
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
	}, true
}
