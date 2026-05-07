package usage

import pkgusage "github.com/wyolet/relay/pkg/usage"

// Tokens is an alias for pkg/usage.Tokens. The canonical definition lives in
// pkg/usage so shape parsers (pkg/api/*) can return it without an internal import.
type Tokens = pkgusage.Tokens
