// Package wire handles conversion between the operator-facing YAML/JSON wire
// format and the domain structs in the app/* entity packages.
//
// Wire format uses entity *names* (DNS-1123 slugs) for cross-references.
// Domain structs use *ids* (UUIDv7). This package bridges the gap.
//
// Typical flow:
//
//  1. Parse raw YAML via Parse or LoadDir → []Document
//  2. Build a Resolver from your name→id index (snapshot, seed index, etc.)
//  3. Call ToXxx(dto, resolver) → domain struct ready for Store.Upsert
//
// Reverse direction (GET responses):
//
//  1. Receive domain struct from store
//  2. Build a ReverseResolver from your id→name index
//  3. Call FromXxx(domain, rev) → wire DTO suitable for JSON/YAML output

// Package manifest converts between the catalog's YAML/JSON wire format, which references entities by name, and the app/* domain structs, which reference them by id. The catalog kinds' DTOs, parsing and name→id translation live in pkg/catalogkind/manifest; this package registers relay's other kinds on top of it and owns rendering.
package manifest

import catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"

// APIVersion is the catalog schema version every relay-aware YAML declares.
const APIVersion = catalogmanifest.APIVersion

// PreviousAPIVersion is still accepted for the kinds v1alpha3 left unchanged.
const PreviousAPIVersion = catalogmanifest.PreviousAPIVersion
