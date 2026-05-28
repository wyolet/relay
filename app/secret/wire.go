package secret

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
	"github.com/wyolet/relay/pkg/secret/aws"
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
// External backends (Vault, AWS SM) register additional kinds here later.
func Wire(q *gen.Queries, pool *pgxpool.Pool, masterKey []byte) (*pkgsecret.Registry, *pkgsecret.StoredResolver) {
	store := NewStore(q, pool)
	stored := pkgsecret.NewStoredResolver(store, masterKey, 1)

	reg := pkgsecret.NewRegistry()
	reg.Register(pkgsecret.KindEnv, pkgsecret.EnvResolver{})
	reg.Register(pkgsecret.KindStored, stored)
	if cfg, err := aws.ConfigFromEnv(); err == nil {
		reg.Register(pkgsecret.KindAWS, aws.New(cfg))
	}
	return reg, stored
}
