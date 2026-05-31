// Package jobq is a self-contained, durable background-job engine backed by
// Postgres. It is a generic primitive: it knows nothing about LLMs, the relay
// catalog, or any caller domain. A job is an opaque input payload plus a queue
// name; a registered Handler turns input bytes into output bytes; jobq owns
// everything else — durable persistence, multi-worker claim, retries, crash
// recovery, and concurrency control.
//
// Why a separate Go module: jobq has its OWN go.mod (import path
// github.com/wyolet/relay/jobq) so the boundary is compiler-enforced — it
// cannot import the relay server (app/, internal/, sdk/) even by accident.
// The server depends on jobq; never the reverse. The first consumer is the
// batch subsystem's inference handler; webhooks delivery is the intended
// second.
//
// Storage split: the Postgres row is a lean envelope (state, attempt counters,
// timestamps, and payload *references*) — it never holds payload bytes. Both
// the input and the result live in a PayloadStore (a file backend by default),
// addressed by URI on the row. This keeps the hot claim table narrow and lets
// huge payloads scale independently of the queue.
//
// Self-owned schema: jobq declares and applies its own migrations via
// Migrate, tracked in its own jobq_schema_migrations table — deliberately
// separate from the relay server's central migrations. This is the one place
// jobq diverges from the server's "SQL lives only in internal/storage" rule,
// justified because jobq is standalone infrastructure, not a relay domain
// entity.
//
// Locking model (after evaluating gue's tx-held-open approach and rejecting it
// for long-running jobs): jobq claims a job by atomically flipping its state to
// 'running' under FOR UPDATE SKIP LOCKED and committing immediately — the
// worker holds no database connection while the handler runs. A crashed worker
// leaves a row stuck in 'running'; a periodic rescuer reclaims any whose
// attempted_at is older than RescueAfter. RescueAfter must exceed JobTimeout so
// the rescuer only ever catches genuinely dead workers, never slow ones.
//
// Out of scope (deliberately, for now): provider-native batch passthrough,
// leader election (the rescuer is idempotent and safe to run on every node),
// pg_notify wakeup (the claim loop polls), and result retention/GC.
package jobq
