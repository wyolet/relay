package catalog

import "context"

// Reloader is an optional capability for Store implementations that support
// hot-swapping their catalog snapshot without restart.
type Reloader interface {
	Reload(ctx context.Context) error
}
