package catalogsource

import (
	"context"
	"strings"
	"sync"

	"github.com/wyolet/relay/sdk/catalog"
)

// Source yields a full catalog.
type Source interface {
	Catalog(ctx context.Context) (*catalog.IndexedCatalog, error)
}

type sourceFunc func(ctx context.Context) (*catalog.IndexedCatalog, error)

func (f sourceFunc) Catalog(ctx context.Context) (*catalog.IndexedCatalog, error) { return f(ctx) }

// Vars rather than direct calls so tests can stand in an embed that carries a release tag.
var (
	loadEmbedded    = catalog.Load
	embeddedVersion = sync.OnceValue(func() string {
		ic, err := loadEmbedded()
		if err != nil {
			return ""
		}
		return releaseTag(ic.Catalog.Version)
	})
)

// Embedded is the catalog compiled into this SDK version.
func Embedded() Source {
	return sourceFunc(func(context.Context) (*catalog.IndexedCatalog, error) { return loadEmbedded() })
}

// EmbeddedVersion is the relay-catalog release tag the embedded catalog was generated from, or "" when the embed records none.
func EmbeddedVersion() string { return embeddedVersion() }

// releaseTag returns version when it is a release tag. An embed generated without a tag records the schema label "relay-catalog@<channel>" instead, which must never match a pinned tag.
func releaseTag(version string) string {
	if strings.Contains(version, "@") || checkTag(version) != nil {
		return ""
	}
	return version
}

// File is a local catalog JSON file, gzip or plain, as cmd/catalog-embed writes it.
func File(path string) Source {
	return sourceFunc(func(context.Context) (*catalog.IndexedCatalog, error) { return catalog.LoadFile(path) })
}
