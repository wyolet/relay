// Package usagelog is the first consumer of pkg/lifecycle's Hook +
// Collector surface. Per-request usage records — pure attribution +
// token counts.
//
// Four pieces, split along produce → attach → store:
//
//   - Event: the canonical usage schema. Stable JSON shape — sinks
//     (file JSONL today, ClickHouse later) consume by deserializing
//     this struct directly. No cost, no pricing — those are downstream
//     consumer concerns. The event answers "who called what model on
//     which host and how many tokens." Cost layers on at query time.
//   - UsageHook: the lifecycle.Hook (producer). Parses tokens + finish
//     reason out of the response body via v1.ExtractSummary, builds the
//     Event, returns it. Pure — the Registry attaches it to the Context
//     under Namespace; the hook never touches the Context.
//   - SinkCollector: the lifecycle.Collector (janitor / store). Reads the
//     attached Event back off the Context and pushes it onto the Emitter.
//   - Emitter: bounded-queue dispatcher. One drain goroutine writes to
//     each Sink. Drop-on-full with counter — never blocks the
//     post-flight goroutine.
//
// The split is what lets a pre-send reader (usage echo) and the sink
// share one collection: the hook produces once, the Registry attaches,
// and any number of readers/collectors consume it.
package usagelog
