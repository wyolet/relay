// Package usagelog is the first consumer of pkg/lifecycle's PostFlightHook
// surface. Per-request usage records — pure attribution + token counts.
//
// Three pieces:
//
//   - Event: the canonical usage schema. Stable JSON shape — sinks
//     (file JSONL today, ClickHouse later) consume by deserializing
//     this struct directly. No cost, no pricing — those are downstream
//     consumer concerns. The event answers "who called what model on
//     which host and how many tokens." Cost layers on at query time.
//   - Emitter: bounded-queue dispatcher. One drain goroutine writes to
//     each Sink. Drop-on-full with counter — never blocks the
//     post-flight goroutine.
//   - Hook: the lifecycle.PostFlightHook implementation. Resolves the
//     per-binding token extractor, parses tokens out of the response
//     body, builds the Event, queues it.
//
// Dependencies are explicit: an AdapterResolver (for token extraction)
// is injected at construction. No globals, no singletons.
package usagelog
