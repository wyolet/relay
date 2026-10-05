package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/kv"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
	secretoauth "github.com/wyolet/relay/pkg/secret/oauth"
)

func openKV(bootCtx context.Context, cfg *config.Config) kv.Store {
	// kv backend — sessions, rate-limits, key-pool all share this.
	var kvStore kv.Store
	if cfg.StateBackend == "redis" {
		if cfg.RedisAddr == "" {
			slog.Error("RELAY_REDIS_ADDR required when RELAY_STATE_BACKEND=redis")
			os.Exit(1)
		}
		rs, err := kv.NewRedis(bootCtx, kv.RedisConfig{
			Addr:         cfg.RedisAddr,
			Password:     cfg.RedisPassword,
			PoolSize:     cfg.RedisPoolSize,
			MinIdleConns: cfg.RedisMinIdleConns,
		})
		if err != nil {
			slog.Error("state(redis) init failed", "err", err)
			os.Exit(1)
		}
		kvStore = rs
	} else {
		kvStore = kv.NewMem()
	}
	return kvStore
}

func startOAuthRefresher(listenerCtx context.Context, st *storagemod.Storage, stores *appcatalog.Stores, kvStore kv.Store) {
	// Proactive OAuth renewal: keeps subscription tokens fresh ahead of
	// expiry so requests never pay refresh latency. Cluster-safe via the kv
	// lock; outcomes persist on the hostkey status (UI-visible on the same
	// key) and broadcast through the catalog hostkey NOTIFY so every pod
	// reloads the credential (secret_values has no trigger of its own).
	oauthRefs := func(ctx context.Context) ([]pkgsecret.Ref, error) {
		keys, err := stores.HostKey.List(ctx)
		if err != nil {
			return nil, err
		}
		var refs []pkgsecret.Ref
		for _, k := range keys {
			if k.Spec.ValueFrom.Kind != hostkey.ValueKindOAuth {
				continue
			}
			// Revoked grants wait for operator re-auth (a value update
			// clears the status) — the refresher contract excludes them.
			if c := k.Status.Credential; c != nil && c.State == hostkey.CredentialRevoked {
				continue
			}
			refs = append(refs, pkgsecret.Ref{
				Kind: pkgsecret.KindOAuth, ID: k.Meta.ID, Provider: k.Spec.ValueFrom.Provider,
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
