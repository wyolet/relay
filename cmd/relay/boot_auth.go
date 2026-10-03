package main

import (
	"context"
	"log/slog"
	"os"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settingswatch"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/identity"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
)

// builtinRoleSeedLock is the advisory-lock id the built-in role seed
// serializes on. Arbitrary but fixed: every pod must pick the same number.
const builtinRoleSeedLock int64 = 0x52454C41595F5242

func loadUsers(bootCtx context.Context, cfg *config.Config, st *storagemod.Storage) (*identity.Store, *user.Store) {
	// Identity store — fatal if YAML is malformed (login would silently
	// be disabled otherwise). Empty store is fine (login returns 503).
	idStore, err := identity.LoadYAML(cfg.ConfigDir)
	if err != nil {
		slog.Error("identity: load YAML failed", "err", err)
		os.Exit(1)
	}
	if n := len(idStore.Users()); n > 0 {
		slog.Debug("identity: loaded users", "count", n)
	}

	// DB-backed users: login reads the table; YAML identity is the
	// seed-if-absent bootstrap (and break-glass fallback at login).
	usersStore := user.NewStore(gen.New(st.Pool()))
	if err := user.SeedFromIdentity(bootCtx, usersStore, idStore, slog.Default()); err != nil {
		slog.Error("user seed from identity YAML failed", "err", err)
		os.Exit(1)
	}
	return idStore, usersStore
}

func buildTokenSigning(bootCtx context.Context, cfg *config.Config, st *storagemod.Storage, stores *appcatalog.Stores) (*control.TokenSigner, *inference.TokenVerifier) {
	// Inference tokens: the signing key is generated on first boot and kept
	// under the master key; both planes hold it in memory and follow the
	// auth:tokens section from there.
	tokenSigner := &control.TokenSigner{}
	tokenVerifier := &inference.TokenVerifier{}
	if err := loadTokenSigningKey(bootCtx, st.Pool(), stores, cfg.MasterKey, tokenSigner, tokenVerifier); err != nil {
		slog.Error("auth: inference-token signing key unavailable", "err", err)
		os.Exit(1)
	}
	return tokenSigner, tokenVerifier
}

func watchAuthTokens(listenerCtx context.Context, cfg *config.Config, st *storagemod.Storage, stores *appcatalog.Stores, cat *appcatalog.Catalog,
	tokenSigner *control.TokenSigner, tokenVerifier *inference.TokenVerifier) {
	settingswatch.New(cat, settings.AuthTokensSection, func(a settings.AuthTokens) {
		if err := applyAuthTokensSection(listenerCtx, st.Pool(), stores, cfg.MasterKey, a, tokenSigner, tokenVerifier); err != nil {
			slog.Error("auth: inference-token signing key reload failed", "err", err)
		}
	}, slog.Default()).Start()
}

func seedBuiltinRoles(bootCtx context.Context, st *storagemod.Storage, stores *appcatalog.Stores) {
	// Built-in roles: seed-if-absent, so an operator's edits survive and a
	// fresh deployment always has the seven system rows to bind against.
	seedLock := func(ctx context.Context, fn func(context.Context) error) error {
		return storagemod.WithAdvisoryLock(ctx, st.Pool(), builtinRoleSeedLock, fn)
	}
	if err := role.SeedBuiltins(bootCtx, stores.Role, slog.Default(), seedLock); err != nil {
		slog.Error("built-in role seed failed", "err", err)
		os.Exit(1)
	}
}

func validateOIDCEnv() {
	// WYOLET_* OIDC env overlay: validate at boot so a typo'd overlay fails
	// the boot, not the first login attempt.
	if oidcEnv, err := settings.AuthOIDCEnv(); err != nil {
		slog.Error("auth: invalid WYOLET_* OIDC env overlay", "err", err)
		os.Exit(1)
	} else if oidcEnv != nil {
		slog.Info("auth: oidc login enabled via WYOLET_AUTH_MODE",
			"issuer", oidcEnv.Issuer, "registration", oidcEnv.Registration)
	} else if mode := os.Getenv("WYOLET_AUTH_MODE"); mode != "" && mode != "oidc" {
		slog.Warn("auth: WYOLET_AUTH_MODE not implemented by relay; password login remains the no-IdP path", "mode", mode)
	}
}
