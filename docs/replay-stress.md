# Replay stress: CC session history through fake-Anthropic

Status: proposal (2026-06-11). Companion repo: `wyolet/spec-mock-anthropic`
(to be created, mirroring `spec-mock-openai` idioms).

## Goal

Replay all local Claude Code session history (~739 sessions, 489 MB)
through relay against a fake Anthropic upstream that returns the
recorded responses verbatim. One run produces two things:

1. **Stress signal** — closed-loop load (each session is a serial
   turn chain ⇒ N sessions = N concurrent *users*) that exercises the
   Anthropic adapter, streaming tee, post-flight fan-out, Redis Lua
   admission, and keypool under real-world traffic shape.
2. **Usage data** — months of real token distributions (cache-heavy
   prefixes, opus/fable mixes, tool storms) landing in the usage sink
   with their **original timestamps**, to design the relationship-story
   UI against.

Runs on devstack only. Session content is private — fixtures never
leave the LAN, never get published as a corpus.

## Components

```
cmd/ingest   (spec-mock-anthropic)  CC ~/.claude/projects jsonl → fixture jsonl
mock server  (spec-mock-anthropic)  serves fixtures as /v1/messages, SSE re-synthesis
cmd/replay   (spec-mock-anthropic)  driver: reconstructs requests, fires at relay
relay change (this repo)            X-WR-Event-Time trusted header +
                                    X-WR-Request-Tags + stress metrics
```

### Ingester: CC session jsonl → fixtures

CC writes one jsonl line **per content block**, not per API response
(observed: 1840 assistant lines → 1164 distinct `message.id`s in one
session). Ingest groups assistant lines by `message.id`; each group
becomes one fixture turn carrying:

- the full recorded `message` (content blocks merged in line order)
- `usage` verbatim (incl. `cache_creation` TTL split, `server_tool_use`)
- `model`, `stop_reason`, original `timestamp` (first line of group)
- coarse pacing anchors: the per-block line timestamps

Skip rules (tally + report, never abort):

- malformed jsonl lines (the "corrupted sessions")
- `model: "<synthetic>"` (CC-internal, no API call behind it)
- non-Anthropic models (`gpt-oss:*`, `gemma*` — ollama-routed turns;
  out of scope for this mock)
- sidechain/agent records keep their own turn chains (they were real
  parallel API traffic — good for stress; group by `sessionId` +
  sidechain lineage)

### Request reconstruction + matching

The driver reconstructs each turn's request (accumulated messages +
system + tools, approximated from the transcript). Fidelity is loose
by design — usage comes from the recorded response, the request only
needs to pass relay's parse path.

**Matching cannot be body-equality** (spec-mock-openai's
`body_json_equals`): when relay runs the translation chain it
re-serializes the body, so bytes differ. Instead the driver embeds a
marker in the last message's text — `⟦replay:<session>:<turn>⟧` —
which survives parse → canonical → serialize untouched. The mock
matches by scanning the body for the marker. Stateless, concurrent-
safe, corruption-tolerant (unmatched request ⇒ 404 + logged as a
replay bug). Token realism is unaffected: tokens come from the
recorded response, never recomputed.

This also lets one run mix both dispatch paths:

- **byte-pass leg**: inbound anthropic → anthropic binding (same-shape
  `io.Copy`)
- **translation leg**: inbound openai shape → anthropic binding (full
  canonical chain) — the adapter stress. Driver flag picks the leg
  (or splits sessions across both).

### Streaming re-synthesis

Sessions store final messages, not SSE streams. The mock re-emits a
valid Anthropic event stream per fixture: `message_start` (with
usage) → `content_block_start/delta/stop` per block (text chunked,
tool_use args chunked as `input_json_delta`, thinking as
`thinking_delta`) → `message_delta` (stop_reason + output_tokens) →
`message_stop`.

Pacing, driver-selected per run:

- **blast** — no delays; throughput / contention ceiling test
- **paced** — inter-chunk delays interpolated from the per-block line
  timestamps; tests sustained concurrent-open-streams (memory, fds,
  tee buffers — a different bottleneck class)

### Trusted event-time header (relay change)

- Header: `X-WR-Event-Time` (RFC3339). Already stripped before
  upstream by the existing `X-WR-*` denylist — no leak path.
- Honored **only** when `RELAY_DEV_TRUST_EVENT_TIME=1`. Default off;
  never documented in operator-facing config; ignored silently when
  the flag is unset.
- Mechanics: do **not** touch `Timing.Start` — every duration is
  computed `sinceStart()` and must stay real. Add
  `lifecycle.Context.EventTime time.Time` (zero = unset), set by the
  inference handler from the header under the flag. `app/usagelog`
  stamps `Event.Timestamp = EventTime` when set, else `Timing.Start`
  (today: `app/usagelog/hook.go:47`).
- Result: a blast run finishes in minutes, latency/duration metrics
  are real, and the usage timeline lands on the original session
  dates.

### Request tags header (relay change)

Caller-supplied observability tags, opaque to relay:

```
X-WR-Request-Tags: {"session_id":"0988342e-…","leg":"translate"}
```

- Flat JSON object, string→string. Not a dev flag — a normal,
  always-on obs feature (this is how callers get session/trace
  grouping in usage data; relay does attribution, never session
  *inference*).
- **Zero hot-path cost**: the inference handler copies the raw header
  string into `lifecycle.Context.Metadata`; parsing + validation
  happen in the post-flight goroutine (usagelog hook). Invalid JSON,
  non-flat values, oversized payloads ⇒ tags silently dropped, the
  request is never affected. Caps: 2 KB header, ≤16 keys, ≤64-char
  keys / ≤256-char values.
- Lands as `Event.Tags map[string]string` — a **separate field**, not
  `Extras`. `Extras` stays relay-stamped (client_ip, …);
  `Tags` is caller-owned. No key collisions, and queries can trust
  the provenance of each.
- Queryable: JSONL via jq today; the usage read API grows
  `tags.<key>` filter + group_by support (same engine that got
  error_kind/finish_reason); ClickHouse sink maps to
  `Map(String,String)`, PG to `jsonb`.
- Never a Prometheus label (unbounded cardinality). Filter/group in
  the usage API only.
- Stripped before upstream by the existing `X-WR-*` denylist.

The replay driver sets `session_id` (the CC sessionId),
`leg` (`bytepass|translate`), and a `replay_run` id — which gives the
relationship-story UI real per-session grouping ("this CC session
cost $X") and lets us slice or bulk-delete a run's events afterwards.
Live CC dogfooding can set a static tag via `ANTHROPIC_CUSTOM_HEADERS`
(≈ one session per CC process).

## Metrics: what we watch during the burn

### Already shipped (watch, no work)

| Metric | Question it answers during the run |
|---|---|
| `relay_requests_total{source,status}` | throughput + error rate; non-2xx spikes = first failure mode |
| `relay_overhead_seconds{source}` | the SLO metric — does p99 hold under N concurrent users? |
| `relay_request_seconds{source}` | total incl. upstream; sanity vs mock pacing |
| `relay_records_lost_total{kind}` | **expected first creak** — usage/payload emitter drop-on-full |
| `relay_provider_keys_down_total{reason}` | breaker trips (should be ~0; nonzero = mock misbehaving or contention bug) |
| `go_goroutines` | leak / runaway detector under mass stream-close |
| `process_open_fds` | concurrent-open-streams ceiling |
| `process_resident_memory_bytes`, `go_memstats_heap_*` | tee-buffer memory under paced mode |
| `go_gc_pause_*`, `go_memstats_gc_cpu_fraction` | GC pressure from per-request buffering |

### To add (one relay PR, before the first burn)

Per `docs/metrics.md` philosophy — each earns its place by answering
a stress question we will actually ask:

| Metric | Type | Why |
|---|---|---|
| `relay_inflight_requests{source}` | gauge | "how many streams are open right now / where does concurrency plateau?" — the closed-loop saturation signal. inc at accept, dec in post-flight trigger. |
| `relay_admission_seconds{source}` | histogram | `Timing.Start → Upstream.Start`: auth + rate-limit Lua reserve + key selection. Directly measures Redis contention without instrumenting `pkg/kv` — if this spikes, *then* we drill into kv/Lua with a one-off. |
| `relay_post_flight_seconds` | histogram | "is post-flight keeping up with stream closes, or building a goroutine backlog?" The long-pending post-flight duration metric; marks from the detached goroutine, never blocks. |
| `relay_emit_queue_depth{kind}` | gauge (GaugeFunc on chan len) | drops (`records_lost`) tell us *after* it's too late; depth shows approach to saturation and whether sink draining keeps up. |

Deliberately **not** added (revisit only if `admission_seconds` points
there): per-op kv latency, Lua script timing, per-hook lifecycle
timing, tee-buffer byte gauges (RSS covers it coarsely).

### Logged, not metered

- **Driver summary** (JSON, end of run + periodic progress lines):
  sessions total/done/failed, turns replayed, corrupt lines skipped
  (by reason), wall time, effective req/s, per-leg (byte-pass vs
  translation) counts, response error tally by status.
- **Mock**: unmatched-marker requests (each one is a replay bug),
  malformed-fixture loads. Fixture stats at startup (sessions, turns,
  models histogram).
- **Relay**: existing structured logs; usage sink output is itself
  the artifact under test.

## Devstack run plan

1. rsync `~/.claude/projects/**/*.jsonl` → devstack (private path).
2. Build + run ingester → fixture dir; review skip tally.
3. Start mock; seed relay catalog: host `anthropic-mock` (upstream →
   mock addr), bindings for `claude-opus-4-7`, `claude-opus-4-8`,
   `claude-fable-5` (adapter `anthropic`), pricing rows so cost
   computation has data, policy + relay key for the driver. Second
   policy/bindings for the translation leg.
4. `RELAY_DEV_TRUST_EVENT_TIME=1` on the relay pods only for this
   stack.
5. Burn order: one session (smoke) → 10 → all 739 blast → all paced.
   `make breakers-reset` between misconfigured runs.
6. Watch: Prometheus scrape of relay `/metrics` (control port) +
   driver progress log. Grafana panel set over the table above if we
   want it pretty; `curl + promtool` is enough for round one.
7. Artifact: usage sink rows with original timestamps → point the
   usage UI at it.

## Open questions

- Reconstruction of system prompt + tools from transcripts is lossy
  (CC doesn't store the raw outbound request). Accepted — affects
  request realism only, not usage numbers. If we later want exact
  request bodies, CC's `--debug` API logging or a recording proxy
  session is the path.
- Subscription pricing: these sessions ran on a Max plan, so catalog
  pricing produces *shadow* cost ("what this would have cost on API")
  — exactly the cost-wedge story, but the UI should label it as
  reference rate.
- Whether `relay_inflight_requests` should also label the dispatch
  leg (`bytepass|translate`) — decide in the PR; cardinality is 2.
- Whether CC populates Anthropic `metadata.user_id` with something
  session-derived — if so, the adapter could lift it into event tags
  at parse time and real CC traffic gets session grouping with zero
  client config. Check against live traffic once CC runs through
  relay.
