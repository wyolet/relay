// Package catalogserver serves relay-catalog releases over plain HTTP from a
// git mirror of the catalog repository.
//
// Each release tag's two files (the data tarball and catalog.json.gz) are
// rebuilt from the tagged tree and served only when their sha256 matches
// the sum published with the tag's release; otherwise the published file
// itself is fetched, checked against its sum, and served. The channel index
// is derived from the tags, never copied, so it can't name a tag whose files
// aren't published and verified. The same Build steps produce the files the
// release publishes, so CI and the server share one builder.
//
// Out of scope: unreleased commits and branches, pre-release tags, webhooks,
// and signing. The server holds no credentials and no state beyond the
// mirror directory and memory.
package catalogserver
