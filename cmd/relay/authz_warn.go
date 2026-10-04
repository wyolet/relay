package main

import (
	"log/slog"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/config"
)

// warnSingleUserOpenRegistration flags a pairing that hands admin to anyone
// the identity provider knows: single-user authorization treats every
// authenticated caller as the operator, and open registration lets any IdP
// user become one.
func warnSingleUserOpenRegistration(authzMode string, oidc *settings.AuthOIDC) {
	if authzMode != config.AuthzSingle || oidc == nil || !oidc.Enabled || !oidc.OpenRegistration() {
		return
	}
	slog.Warn("auth: RELAY_AUTHZ=single with open OIDC registration — every identity-provider user who signs in is an admin; set RELAY_AUTHZ=rbac or registration=closed")
}
