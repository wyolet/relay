# Logs + payload — the unified per-request record

There is **one logical event per request** — the *log* (what happened:
routing, status, timing, tokens, errors, identity). It's the `usage.Event`,
produced by the constant log observer for every request. **Payload logging**
adds exactly one optional thing to it: the captured request/response
**bodies**, for opted-in requests.

Two API projections over that one record:

- **`/usage`** — the narrow token/consumption metrics (billing).
- **`/logs`** — the full record, with the captured bodies attached when
  payload logging was opted in (`payload: null` / "(not logged)" otherwise).

Storage is **two independent knobs**, joined by `request_id`:

- the **log/usage store** (file / clickhouse / postgres / valkey) — all the
  metadata;
- the **payload body store** (file / s3 / clickhouse) — **body-only**: the
  request/response bytes keyed by `request_id`, plus a timestamp (for
  partition/TTL) and truncation flags. **No metadata is duplicated here** —
  there is no separate payload-metadata schema, because every metadata field
  already lives on the log event.

The two observers attach to the lifecycle `Registry` on **separate emitters**
(big bodies must not starve log metrics); nothing else connects logging to
the runtime — the hot path only mints the `Context` and sets
`lc.PayloadLog`.

This doc covers the write side (capture → sink, shipped PRs #225/#227) and
the read side (the `/logs` Logs API).

## Opt-in + gating

Capture is gated by three independent switches, all of which must be on:

1. **Global master switch** — `payload-logging` settings section,
   `Enabled`. Hot-swappable.
2. **Per-request opt-in** — either the matched `Policy` or the
   authenticating `RelayKey` sets `PayloadLoggingEnabled`. Resolved at the
   inference entry into `lifecycle.Context.PayloadLog`.
3. **A sink exists** — the backend is configured and built.

Off by default. The per-request opt-in means an operator can capture a
single noisy policy without logging everything.

## Write side — observer → emitter → sink

- **Observer** (`app/payloadlog`): a buffered `PayloadHook` (fills a
  `Record` from the lifecycle Context + response body at post-flight) and
  a streaming `StreamPayloadFactory` (accumulates SSE frames, builds the
  record at end-of-stream). Both gate on `lc.PayloadLog && Controller.Enabled()`
  and clip bodies to `MaxBytes`.
- **Emitter**: a bounded, drop-on-full channel (`DefaultQueueSize = 256`)
  draining on a single goroutine — never blocks the response.
- **Sink** (`pkg/payload` + `pkg/payload/<backend>`): the `Record` is
  written to the configured backend.
- **Controller** (`app/payloadlog/controller.go`): owns the live config,
  hot-swaps the sink (toggle / backend / bucket / credentials) on a
  settings change with no restart.

### Record shape

`pkg/payload.Record` is **body-only**: `RequestID`, `Timestamp` (for
partition/TTL/keying), the `RequestBody`/`ResponseBody` bytes, and the
`RequestTruncated`/`ResponseTruncated` flags. Nothing else — every
per-request metadata field lives on the log event (`usage.Event`) and is
read via `/logs`, joined by `request_id`. The body store carries no
duplicate of it.

## Backends

Selected by `payload-logging.backend`:

| Backend | Where | Notes |
|---|---|---|
| `file` (default) | one JSONL file, bodies base64 | dogfood / single node |
| `s3` | one object per record, `<prefix>/YYYY/MM/DD/<request_id>.json` | build-tagged out of `-tags minimal` (minio-go) |
| `clickhouse` | one body-only `payload_logs` MergeTree row, bodies as ZSTD `String` columns | production; reuses the relay's CH cluster (`RELAY_CH_DSN`) |

### Why ClickHouse for bodies (the Langfuse model)

The instinct "don't put blobs in ClickHouse" is too strong for **text**
payloads. LLM wire bodies are JSON, and:

- **ZSTD compresses them ~10×**, and CH compresses column data in **blocks**
  — so the near-identical request bodies of a multi-turn chat session
  (each turn resends the growing history, O(N²) raw) collapse to roughly
  O(N) on disk. The duplication that worries you is exactly what compresses
  away.
- Bodies stay **queryable and displayable** next to the metadata — one
  store, no second fetch for the common case.

This is the same split Langfuse v3 uses (traces + input/output text in
ClickHouse; binary media in object storage). The `clickhouse` backend
implements the text half; binary/oversized-media spill to object storage
is a follow-up (see "Roadmap" below and `design/media-offload.md`, which
shares the content-hash primitive).

### CH backend internals

- **Schema** (`payload_logs`): body-only — `request_id`, `ts`,
  `request_body`/`response_body` (`String CODEC(ZSTD(3))`), and the two
  truncation flags. `MergeTree`, `PARTITION BY toYYYYMMDD(ts)`, `ORDER BY
  (ts, request_id)`, `TTL ... INTERVAL <retention> DAY`. A `bloom_filter`
  skip index on `request_id` keeps `Get` (a point lookup with no time bound
  → no partition pruning) from scanning the whole table. `CREATE TABLE IF
  NOT EXISTS` + a `system.columns` check fails fast on a pre-existing
  incompatible table rather than auto-dropping.
- **Durability**: a WAL-segment queue (mirrors the usage CH sink) — records
  append to an active segment; full segments rotate and flush to CH; a
  segment is deleted only after CH confirms; leftover segments replay on
  boot. **Unlike** usage, rotation also triggers on a **byte threshold**
  (default 64 MiB) because bodies are MB-scale — a line-count cap alone
  would let a segment grow to gigabytes.
- **Config**: the DSN reuses `RELAY_CH_DSN` (the same cluster the usage
  sink uses; a separate `payload_logs` table), so **no credentials live in
  the settings row**. Only safe knobs (`retentionDays`, `walDir`) are
  hot-swappable settings overrides. Default retention is 30 days (shorter
  than usage's 90 — bodies are bulky and short-lived).

## Read side — the `/logs` API

The Logs page reads the **log store**, not the body store — the list never
needs the bodies, and the log store already has all the metadata. Two
control-plane endpoints (session/admin auth via `Authz`):

```
GET /logs                full lifecycle records, filtered + keyset-paginated
GET /logs/{request_id}   one record + its captured bodies (payload null if not logged)
```

- **`GET /logs`** is served by the **log (usage) reader** — filters (time
  window, `relay_key_hash`/`policy_id`/`model_id`/`host_id`/`source`/
  `error_kind`, status range) and keyset cursor on `(ts DESC, request_id
  DESC)` all run against the log store. No body store touched.
- **`GET /logs/{request_id}`** fetches the log record (log reader) and, if
  payload logging was opted in, attaches the bodies via the **body store
  `Get(request_id)`** (`payload.Reader`). Body absence is normal (opt-in),
  not an error — `payload` is simply null.

`/usage/*` stays the narrow metrics projection over the same log store.

### Body store read characteristics (Get only)

The body store is fetched **by request_id only** — there is no list/filter
on it (that's the log store's job). `Get`:

| Backend | Get |
|---|---|
| `clickhouse` | indexed point lookup (bloom filter on `request_id`); bodies decompressed |
| `file` | linear JSONL scan for the id (dogfood scale) |
| `s3` | recursive `ListObjects` for the `<request_id>.json` suffix, then fetch |

### Wiring

The sink (write) and reader (read) are **separate lifecycles**: the
`Controller` builds and hot-swaps the sink from settings; the
`payloadReaderResolver` (cmd/relay) builds the `Get`-only reader lazily,
rebuilding when the live backend config changes and closing the previous
reader (the CH reader holds a connection pool). For CH the read path uses a
**connection-only `NewReader`** — no WAL, since it only queries.

## Roadmap

- **Media spill to object storage**: oversized/binary bodies (base64
  images/audio) don't compress and bloat CH rows. Spill them to S3 by
  content-hash, store the URI in the CH row, fetch on `Get`. Shares the
  content-hash primitive with `design/media-offload.md`.
- **Content-addressed message dedup**: hash each message, store unique
  messages once, store a request as a list of hashes — turns the O(N²)
  chat-history redundancy into O(N) for real (vs. relying on compression).
  Worth it only at high volume.

- **Real-time log streaming (live tail).** The query store (CH) is batched
  by design (~10s WAL flush) — it serves history, not a live feed. Live
  tail is a *separate consumer* of the same lifecycle event stream, not a
  schema change. It splits three ways, in increasing cost:
  - **Live tail of completed events** (the feed scrolls as requests
    finish) — cheap: add a real-time **fanout observer** alongside the
    batch sink that pushes each completed log event to a Redis stream /
    SSE channel the control plane re-broadcasts. The lifecycle spine
    already fans out to N sinks, so this is sink N+1.
  - **In-flight visibility** (watch a request *while* it streams) — the
    log event finalizes at **post-flight** (`Body.Close()`), so a long
    stream's log lands seconds later; post-flight logs *cannot* show
    in-flight requests. This needs a separate in-flight/span registry —
    its own subsystem, only if "currently-running" view is a real
    requirement.
  - **CH for real-time** — explicitly *not* a goal. CH is the
    history/query store; don't make it serve the tail.

  WS fits the per-request log model already (each frame = its own
  per-request lifecycle event via the synthetic ResponseWriter); the only
  WS-specific work for capture is accumulating response **frames per
  correlation-id** rather than per-HTTP-body.
