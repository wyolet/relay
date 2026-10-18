//go:build !cgo

package secret

import "github.com/wyolet/relay/secret"

// registerOnePassword is a no-op in CGO_ENABLED=0 builds (relay's default):
// the onepassword-sdk-go package can't compile without CGO, so the 1Password
// backend is excluded. Build with CGO enabled to include it.
func registerOnePassword(_ *secret.Registry) {}
