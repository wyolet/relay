// Package celllink carries the link changes of a published generation from
// package engine to package cell. It exists so neither package exports a
// method only the other calls; nothing outside pkg/engine can import it.
package celllink

import "time"

// Publish is installed by package cell at init. registry is the value the
// engine was given as its cell linker; linked and unlinked are []cell.Key:
// the keys whose link count went from zero to positive and back in this
// generation.
var Publish func(registry any, linked, unlinked any, now time.Time)
