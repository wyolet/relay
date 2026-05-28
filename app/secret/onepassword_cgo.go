//go:build cgo

package secret

import (
	pkgsecret "github.com/wyolet/relay/pkg/secret"
	"github.com/wyolet/relay/pkg/secret/onepassword"
)

// registerOnePassword registers the 1Password resolver when its env config is
// present. Only built into cgo builds — the official onepassword-sdk-go does
// not compile under CGO_ENABLED=0 (its desktop-integration path forces CGO).
func registerOnePassword(reg *pkgsecret.Registry) {
	if cfg, ok := onepassword.ConfigFromEnv(); ok {
		reg.Register(pkgsecret.KindOnePassword, onepassword.New(cfg))
	}
}
