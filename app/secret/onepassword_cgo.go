//go:build cgo

package secret

import (
	"github.com/wyolet/relay/pkg/secret/onepassword"
	"github.com/wyolet/relay/secret"
)

// registerOnePassword registers the 1Password resolver when its env config is
// present. Only built into cgo builds — the official onepassword-sdk-go does
// not compile under CGO_ENABLED=0 (its desktop-integration path forces CGO).
func registerOnePassword(reg *secret.Registry) {
	if cfg, ok := onepassword.ConfigFromEnv(); ok {
		reg.Register(secret.KindOnePassword, onepassword.New(cfg))
	}
}
