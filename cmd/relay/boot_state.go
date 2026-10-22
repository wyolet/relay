package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/kv"
	secretoauth "github.com/wyolet/relay/pkg/secret/oauth"
	"github.com/wyolet/relay/secret"
)

// openKV opens the shared kv store (sessions, admission, key pool and the rest) and the post-flight store that settles served requests. On Redis the post-flight store is a second client with its own pool and longer waits, so admission saturating the shared pool cannot drop a served request's commit; in memory both are one store.
func openKV(bootCtx context.Context, cfg *config.Config) (shared, postFlight kv.Store) {
	if cfg.StateBackend != "redis" {
		mem := kv.NewMem()
		return mem, mem
	}
	if cfg.RedisAddr == "" {
		slog.Error("RELAY_REDIS_ADDR required when RELAY_STATE_BACKEND=redis")
		os.Exit(1)
	}
	base := kv.RedisConfig{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DialTimeout:  cfg.RedisDialTimeout,
		WriteTimeout: cfg.RedisWriteTimeout,
		MaxRetries:   cfg.RedisMaxRetries,
	}
	sharedCfg := base
	sharedCfg.Pool = "shared"
	sharedCfg.PoolSize, sharedCfg.MinIdleConns = cfg.RedisPoolSize, cfg.RedisMinIdleConns
	sharedCfg.ReadTimeout, sharedCfg.PoolTimeout = cfg.RedisReadTimeout, cfg.RedisPoolTimeout
	postCfg := base
	postCfg.Pool = "post-flight"
	postCfg.PoolSize = cfg.RedisPostFlightPoolSize
	postCfg.ReadTimeout, postCfg.PoolTimeout = cfg.RedisPostFlightReadTimeout, cfg.RedisPostFlightPoolTimeout

	rs, err := kv.NewRedis(bootCtx, sharedCfg)
	if err != nil {
		slog.Error("state(redis) init failed", "err", err)
		os.Exit(1)
	}
	post, err := kv.NewRedis(bootCtx, postCfg)
	if err != nil {
		_ = rs.Close()
		slog.Error("state(redis) post-flight client init failed", "err", err)
		os.Exit(1)
	}
	return rs, post
}

func startOAuthRefresher(listenerCtx context.Context, st *storagemod.Storage, stores *appcatalog.Stores, kvStore kv.Store) {
	// Proactive OAuth renewal: keeps subscription tokens fresh ahead of
	// expiry so requests never pay refresh latency. Cluster-safe via the kv
	// lock; outcomes persist on the hostkey status (UI-visible on the same
	// key) and broadcast through the catalog hostkey NOTIFY so every pod
	// reloads the credential (secret_values has no trigger of its own).
	oauthRefs := func(ctx context.Context) ([]secret.Ref, error) {
		keys, err := stores.HostKey.List(ctx)
		if err != nil {
			return nil, err
		}
		var refs []secret.Ref
		for _, k := range keys {
			if k.Spec.ValueFrom.Kind != hostkey.ValueKindOAuth {
				continue
			}
			// Revoked grants wait for operator re-auth (a value update
			// clears the status) — the refresher contract excludes them.
			if c := k.Status.Credential; c != nil && c.State == hostkey.CredentialRevoked {
				continue
			}
			refs = append(refs, secret.Ref{
				Kind: secret.KindOAuth, ID: k.Meta.ID, Provider: k.Spec.ValueFrom.Provider,
			})
		}
		return refs, nil
	}
	oauthNotify := func(ctx context.Context, id string) error {
		_, err := st.Pool().Exec(ctx, "select pg_notify('catalog_events', $1)", "hostkey:upsert:"+id)
		return err
	}
	oauthHooks := secretoauth.Hooks{
		OnRenewed: func(ctx context.Context, id string, expiresAt time.Time) error {
			now := time.Now().UTC()
			if err := stores.HostKey.SetCredentialStatus(ctx, id, hostkey.CredentialStatus{
				State: hostkey.CredentialOK, ExpiresAt: expiresAt, RenewedAt: now, At: now,
			}); err != nil {
				return err
			}
			return oauthNotify(ctx, id)
		},
		OnRevoked: func(ctx context.Context, id string, cause error) error {
			if err := stores.HostKey.SetCredentialStatus(ctx, id, hostkey.CredentialStatus{
				State: hostkey.CredentialRevoked, LastError: cause.Error(), At: time.Now().UTC(),
			}); err != nil {
				return err
			}
			return oauthNotify(ctx, id)
		},
	}
	go secretoauth.NewRefresher(oauthRefs, stores.OAuthResolver, kvStore, oauthHooks, slog.Default()).
		Run(listenerCtx)
}

// healthChecks names the backends the control plane's /healthz pings; the in-memory kv has nothing to ping.
func healthChecks(st *storagemod.Storage, kvStore kv.Store) map[string]control.Pinger {
	checks := map[string]control.Pinger{"pg": st}
	if p, ok := kvStore.(control.Pinger); ok {
		checks["redis"] = p
	}
	return checks
}
