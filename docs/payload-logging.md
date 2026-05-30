# Payload logging — request/response body capture + the Logs view

Payload logging captures the full request and response **bodies** of
opted-in requests, off the hot path, for debugging and audit. It's the
second `lifecycle` observer (after usage) and the data source for the
frontend **Logs** page.

This doc covers both halves: the write side (capture → sink) that shipped
with PRs #225/#227, and the read side (the Logs API) added on top.

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

`pkg/payload.Record` carries identity fields that **mirror
`usage.Event`** (so a record joins its usage row by `request_id`):
`RequestID, Timestamp, Source, Status, Streamed, RelayKeyHash, PolicyID,
ModelID, HostID, ErrorKind` + the `RequestBody`/`ResponseBody` bytes +
`RequestTruncated`/`ResponseTruncated` flags.

## Backends

Selected by `payload-logging.backend`:

| Backend | Where | Notes |
|---|---|---|
| `file` (default) | one JSONL file, bodies base64 | dogfood / single node |
| `s3` | one object per record, `<prefix>/YYYY/MM/DD/<request_id>.json` | build-tagged out of `-tags minimal` (minio-go) |
| `clickhouse` | one `payload_logs` MergeTree row, bodies as ZSTD `String` columns | production; reuses the relay's CH cluster (`RELAY_CH_DSN`) |

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
is a follow-up (see "Roadmap" below and `docs/media-offload.md`, which
shares the content-hash primitive).

### CH backend internals

- **Schema** (`payload_logs`): `MergeTree`, `PARTITION BY toYYYYMMDD(ts)`,
  `ORDER BY (ts, request_id)`, `TTL ... INTERVAL <retention> DAY`. Bodies
  are `String CODEC(ZSTD(3))`. A `bloom_filter` skip index on `request_id`
  keeps `Get` (a point lookup with no time bound → no partition pruning)
  from scanning the whole table. `CREATE TABLE IF NOT EXISTS` + a
  `system.columns` check fails fast on a pre-existing incompatible table
  rather than auto-dropping.
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

## Read side — the Logs API

A `payload.Reader` seam serves two control-plane endpoints (session/admin
auth via `Authz`, nil-guarded like `/usage/*`):

```
GET /payloads                filtered + keyset-paginated metadata list
GET /payloads/{request_id}   full request + response bodies for one request
```

The split is deliberate and is the whole performance story:

- **`List` never returns bodies.** It's the Logs table. Filters mirror
  `/usage/events` (time window, `relay_key_hash`/`policy_id`/`model_id`/
  `host_id`/`source`/`error_kind`, status range), keyset cursor on
  `(ts DESC, request_id DESC)`. On the CH backend it **projects only the
  metadata columns** — the body columns are never read off disk.
- **`Get` returns the bodies** for one request, for the detail view.

### Backend read characteristics

| Backend | List | Get |
|---|---|---|
| `clickhouse` | SQL filter + LIMIT pushed into CH; metadata columns only | indexed point lookup (bloom filter); bodies decompressed |
| `file` | linear JSONL scan, filter in Go (dogfood scale) | linear scan for the id |
| `s3` | date-partition-narrowed `ListObjects` + per-object fetch to filter (scan-heavy — use CH in production) | object fetch by date-partitioned key |

The flat `file`/`s3` readers must read whole records to filter (their
metadata is inline with the body), so they don't scale — they're the
dogfood/fallback path. The `clickhouse` backend is the production read
path: column projection makes `List` cheap regardless of body size.

### Wiring

The sink (write) and reader (read) are **separate lifecycles** in this
subsystem: the `Controller` builds and hot-swaps the sink from settings;
the `payloadReaderResolver` (cmd/relay) builds the reader lazily, rebuilding
when the live backend config changes and closing the previous reader (the
CH reader holds a connection pool). For CH the read path uses a
**connection-only `NewReader`** — no WAL, since it only queries.

## Roadmap

- **Media spill to object storage**: oversized/binary bodies (base64
  images/audio) don't compress and bloat CH rows. Spill them to S3 by
  content-hash, store the URI in the CH row, fetch on `Get`. Shares the
  content-hash primitive with `docs/media-offload.md`.
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
