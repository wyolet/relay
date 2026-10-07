// Package modeldir keeps a consumer's model list as a directory of YAML files, one self-contained file per model, and loads that directory as a *catalog.IndexedCatalog so Resolve, Cost and the context-window fields work unchanged.
//
// Add copies one model out of a Source (the embedded catalog, a local catalog file, or a relay-catalog GitHub release) and records which catalog it came from; Refresh re-derives every such file from a newer Source and reports what changed; Remove and hand-written files (no source) are the user's. A configured directory is the whole model list: Load never falls back to the full catalog, and a missing or empty directory loads empty with ErrNoModels.
//
// Files are written atomically with a fixed key order and no timestamps, so a refresh that changes nothing leaves the directory byte-identical.
//
// Out of scope: routing across hosts (a file carries one host's rate sheet, the one in pricedBy), host base URLs, merging the directory with any other catalog, and signature checks on release assets beyond sha256 over HTTPS. Sources are sdk/catalogsource's, re-exported here; sdk/catalog stays fetch-free.
package modeldir
