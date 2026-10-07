// Package catalogsource yields a full *catalog.IndexedCatalog from one of three places: the catalog embedded in this SDK version, a local catalog file, or a relay-catalog GitHub release asset fetched by tag and cached on disk. Consumers that pin a catalog tag (modeldir, modelroutes) read it through here, so sdk/catalog itself never touches the network or the disk beyond an explicit LoadFile.
//
// A release whose tag matches the embedded catalog is served from the embed without fetching, so a file pinned to the tag this SDK ships with resolves offline.
//
// Out of scope: signature checks on release assets beyond sha256 over HTTPS, cache eviction, and merging catalogs. A cached tag is trusted forever because release tags are immutable.
package catalogsource
