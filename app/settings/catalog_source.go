package settings

// SectionCatalogSource is the section key for the catalog-source marker —
// the record of which public-catalog version the DB's template rows were
// last seeded from.
const SectionCatalogSource = "catalog-source"

// CatalogSource records the provenance of the seeded catalog. Written by
// the bootstrap seeder after a successful versioned seed; compared against
// RELAY_CATALOG_VERSION at boot to decide whether a re-seed is due. An
// operator can blank Version via the settings API to force a re-seed on
// the next boot. Re-seeds only touch pristine template rows: operator-
// edited (dirty) rows are skipped and overlays re-merge at snapshot load,
// so user changes survive.
type CatalogSource struct {
	// Version is the catalog ref (tag) the last versioned seed ran from,
	// e.g. "v0.1.0". Empty when the catalog was seeded without a version
	// (local dir / embedded first-boot seed) or never seeded.
	Version string `json:"version,omitempty"`

	// SeededAt is when that seed completed (RFC 3339).
	SeededAt string `json:"seededAt,omitempty"`
}

func (c *CatalogSource) Validate() error { return nil }

func init() {
	Register(Section{
		Name:        SectionCatalogSource,
		Description: "Provenance of the seeded public catalog (version marker for boot-time re-seed).",
		Defaults:    func() any { return &CatalogSource{} },
		Decode:      decodeAndValidate[CatalogSource, *CatalogSource],
	})
}
