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
package wire

const APIVersion = "relay.wyolet.dev/v1"
