// Package catalogstore is the Postgres data-access layer for the catalog kinds
// (provider, host, model, pricing, binding, policy, ratelimit). The kinds'
// types and validation live in pkg/catalogkind, free of storage, so tools that
// read the catalog without a database share them. Each store is concrete, with
// no interface declared here; consumers declare their own narrow interface if
// they want a test seam. Other entities keep their stores beside their types.
package catalogstore
