package modeldir

import (
	"context"

	"github.com/wyolet/relay/sdk/catalog"
)

// Source yields a full catalog that Add and Refresh pick models from. It is never merged into Load.
type Source interface {
	Catalog(ctx context.Context) (*catalog.IndexedCatalog, error)
}

type sourceFunc func(ctx context.Context) (*catalog.IndexedCatalog, error)

func (f sourceFunc) Catalog(ctx context.Context) (*catalog.IndexedCatalog, error) { return f(ctx) }

// Embedded is the catalog compiled into this SDK version.
func Embedded() Source {
	return sourceFunc(func(context.Context) (*catalog.IndexedCatalog, error) { return catalog.Load() })
}

// File is a local catalog JSON file, gzip or plain, as cmd/catalog-embed writes it.
func File(path string) Source {
	return sourceFunc(func(context.Context) (*catalog.IndexedCatalog, error) { return catalog.LoadFile(path) })
}
