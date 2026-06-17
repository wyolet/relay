# Roadmap — index

This file is the map + the shared changelog. The open-source core's
backlog lives in [`roadmap-oss.md`](roadmap-oss.md):
open-source the infra-grade core — the wedge is a BYO-key, no-reseller,
Go-fast router that out-performs OpenRouter/LiteLLM on the infrastructure
axis.

Items list **what / why / rough size / where it lives**. When "where"
would take more than a sentence, the item is its own design doc waiting
to be written; the path under `design/` is the placeholder.

## How to pick

Default move: take the top open item from **`roadmap-oss.md`** — the OSS
core is the critical path and everything else builds on it. The order
within the doc is intentional so each item unblocks the next without
orphan work.

When in doubt about a backlog/design-first item, write the design doc
first (`design/<topic>.md`) — surfacing the open questions usually flips
"backlog" to "now" or "icebox."

---

## Recently shipped (shared history)

The foundation every track builds on. Track-specific shipped notes live
in each phase doc.

- **`app/` architecture cutover** (PRs #110–#115).
- **v1alpha2 catalog** (PRs #156–#172, catalog PRs #4–#7) — embedded
  Snapshots, HostBinding.Snapshots filter, Aliases removed, owner
  defaults at translate.
- **OpenAI Responses inbound + cross-shape (Phase 1/1.5)** (PRs #175–#183).
- **Canonical Phase 2** (PRs #185–#189, 2026-05-22) — `sdk/v1/` canonical
  protocol package (narrowed Responses; stateless; `extensions` envelope;
  `provider_data` opaque field); OpenAI + Anthropic vendor adapters target
  canonical via `v1.Translator` (pairwise translators deleted); generic
  `app/adapter/` framework (`Spec` + `Registry`); shape-agnostic dispatch
  (`Deps.CrossShapeHandlers` deleted). Verified via `make smoke-mock` +
  live Claude Code → ollama-self/gpt-oss-120b tool-use round-trip.
- **Canonical client + `/v1/generate`** (PR #195) — transport-agnostic
  client (`sdk/client`), canonical served at `/v1/generate`, vendor-neutral
  `cache_config` anchors.
- **Gemini native adapter** (PRs #201/#203, 2026-05-25) — upstream-only
  `generateContent` Translator; introduced `Spec.UpstreamPathFn` + widened
  `pipeline.Adapter.Call(...,upstreamModel,stream)`.
- **Adapter fidelity audits + fixes** (PRs #201/#202/#203) — per-adapter
  audits in `design/adapters/` + a batch of P0/P1 fixes; proposed the
  no-silent-drops contract (now canonical rule 11).
- **WebSocket transport** (PRs #196 server, #198 client) — `/v1/ws`
  canonical shape over one long-lived connection, multiplexed by id.
- **Lifecycle hook system + usage emit** — `pkg/lifecycle` observability
  spine wired into pipeline + proxy post-flight; `app/usagelog` first live
  observer (PostFlight → bounded drop-on-full Emitter → JSONL Sink).
  Pre-cutover generation (`internal/usage`, `pkg/eventlog`,
  `Request.OnSuccess`, no-op `reqid` span, `X-Relay-Metadata`) deleted.
- **Usage read API** (PRs #221/#224/#223, 2026-05-26) —
  `/usage/{events,summary,timeseries}` over the `usage.Reader` seam.
- **ClickHouse usage sink** (PRs #218/#220) — also Postgres + valkey backends.
- **Echo-usage-in-response** (PRs #216/#217) — `X-WR-Usage: full` → inline
  `relay_usage` block (canonical-inbound only).
- **Payload logging** (PR #225, settings #227, 2026-05-26) — second
  lifecycle observer: full request/response body capture, per-policy/relaykey
  opt-in, off the hot path. Unified log/payload model: `GET /logs` +
  `GET /logs/{request_id}`; file/s3/clickhouse body backends; settings-driven
  hot-swap for both payload and log backends. Full design:
  `design/payload-logging.md`.
- **`pkg/secret` unified resolver** (PR #226) + **five external fetch-only
  backends** (PRs #242–#248: aws/azure/gcp/bitwarden/onepassword; 1Password
  behind `cgo` tag, PR #251).
- **KeyAgent secret failover/heal** (PR #252) — out-of-band re-resolve/heal
  on upstream auth failure; value-hash circuit breaker.
- **Boot YAML→DB settings seed** (PR #254) — seed-if-absent.
- **Per-request timing + reasoning span** — `lifecycle.Timing` (µs offsets
  anchored to start) + reasoning span for canonical-inbound streams (#232).
- **Model aliases (resolution-only)** (2026-06-13) — last-priority
  matchers on `model.Spec.Aliases` (exact + single-`*` wildcards),
  verbatim upstream wire name via `Plan.UpstreamModel()`,
  `extras.resolved_via` usage tagging. Solves the Claude Code
  `claude-fable-5[1m]` 404. Full mechanism: `model-aliases.md`. The
  rejected per-policy arbitrary-model passthrough is parked pending an
  external signal.
- **Catalog overlays** (2026-06-13) — user customization that survives
  re-seed: template/overlay/effective split, load-time merge in
  `app/catalog`, union-merged aliases/tags, write-time reject +
  load-time quarantine, `/models/by-id/{id}/overlay` subresource.
  Model-only in v1; clone-as-overlay (Easy Registration) and
  diff-on-write deferred. Full design: `overlays.md`.
