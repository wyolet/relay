// Package config embeds the relay-internal definitions the binary must carry, so a deployment without this directory on disk still gets them. It holds data only: parsing and seeding live in app/seed, and the composition root passes the bytes in.
package config

import _ "embed"

// SystemRateLimits is ratelimits/system.yaml: the system-owned RateLimit rows the relay looks up by name.
//
//go:embed ratelimits/system.yaml
var SystemRateLimits []byte
