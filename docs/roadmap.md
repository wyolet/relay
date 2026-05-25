# Roadmap

This doc tracks what comes next, in **priority order**.

Items list **what / why / rough size / where it lives**. When "where"
would take more than a sentence, the item is its own design doc
waiting to be written; the path under `docs/` is the placeholder.

## Recently shipped

- **`app/` architecture cutover** (PRs #110–#115) — done.
- **v1alpha2 catalog** (PRs #156–#172, catalog PRs #4–#7) — embedded
  Snapshots, HostBinding.Snapshots filter, Aliases removed, owner
  defaults at translate.
- **OpenAI Responses inbound + cross-shape (Phase 1/1.5)** (PRs #175–#183).
- **Canonical Phase 2** (PRs #185–#189, 2026-05-22):
  - `pkg/relay/v1/` canonical protocol package (narrowed Responses;
    stateless; `extensions` envelope; `provider_data` opaque field).
  - OpenAI + Anthropic vendor adapters target canonical via
    `v1.Translator`. Pairwise translator packages deleted.
  - Generic `app/adapter/` framework (`Spec` + `Registry`). Per-vendor
    `app/adapters/<vendor>/` packages deleted. Dispatch is shape-
    agnostic. `Deps.CrossShapeHandlers` deleted.
  - Verified via `make smoke-mock` + live Claude Code →
    ollama-self/gpt-oss-120b tool-use round-trip.
- **Canonical client + `/v1/generate`** (PR #195) — transport-agnostic
  client (`pkg/relay/client`), canonical served at `/v1/generate`,
  vendor-neutral `cache_config` anchors.
- **Gemini native adapter** (PRs #201 base / #203 fixes, 2026-05-25) —
  `pkg/adapters/gemini/` implements `v1.Translator` for the
  `generateContent` shape. **Upstream-only** (registered as a Spec with
  no `InboundPaths`; reachable via canonical/OpenAI/Anthropic inbound
  through the cross-shape chain). Introduced `Spec.UpstreamPathFn` +
  widened `pipeline.Adapter.Call(...,upstreamModel,stream)` for shapes
  that encode model + stream in the URL path. Follow-ups: native inbound
  Gemini route (needs URL-path `{model}` extraction), catalog host/model
  wiring, fake-Gemini integration test.
- **Adapter fidelity audits + fixes** (PRs #201/#202/#203, 2026-05-25) —
  per-adapter "what maps / silently drops / hardcodes" audits in
  `docs/adapters/`, plus a batch of real P0/P1 fixes the audits surfaced
  (CC `NewFromCanonicalStream` nil-panic, Responses encrypted_content +
  streaming refusal/`failed` + func-call streaming ids, Anthropic
  streaming thinking-signature loss + `Parallel` dead code, Gemini safety
  finishReasons + CallID collisions + structured output + stream args).
  Documented gaps left open: Anthropic `max_tokens` default, Anthropic
  `Output.Format`. Proposed contract: no silent drops (emit / log /
  error) — see `docs/adapters/README.md`.
- **WebSocket transport** (PRs #196 server, #198 client) — `/v1/ws`
  serves the canonical shape over one long-lived connection,
  multiplexing requests by caller-chosen id. Auth + classification
  happen once on the upgrade request (reuses the HTTP middleware chain);
  each frame dispatches through the unchanged `handleShape`/`Dispatch`
  via a synthetic `http.ResponseWriter` (`app/transport/ws`), so
  pipeline + dispatch are untouched. The canonical client
  (`pkg/relay/client`) speaks it via `RelayWS(...)` — a pluggable
  `transport` seam (HTTP default, WS new) under the translator;
  sequential per connection (one in-flight request, conn reused across
  turns). Library: `coder/websocket`. Follow-ups: anthropic/openai WS
  endpoints, concurrent multiplexing from a single client, per-frame
  request-id + OTel span, browser subprotocol auth, in-flight cap as
  env config.
- **Lifecycle hook system + usage emit** — `pkg/lifecycle` (per-request
  `Context`, `PreFlightMiddleware`, `PostFlightHook`, `Registry`) is the
  observability spine, wired into both the pipeline and proxy post-flight
  goroutines. `app/usagelog` is the first live observer (PostFlight hook
  → bounded drop-on-full `Emitter` → JSONL `Sink`). The pre-cutover
  generation it replaced — `internal/usage`, `pkg/eventlog`,
  `Request.OnSuccess`, the no-op `reqid` OTel span, and the
  `X-Relay-Metadata` header — was deleted (PR purge-precutover-observability).

## Now — priority queue

Four items, in this order. Order is intentional: secrets unblocks every
deployment that mandates a non-env secret store; batch is the heaviest
build and the headline differentiator; webhooks unlock async UX once
batch lands; new adapters broaden the upstream surface.

### 1. Pluggable secret backends

What: extract `pkg/secret` with a `Resolver` interface. Built-in
resolvers: `env` (env-ref, today) and `stored` (AES-GCM in PG, today).
Add subpackages for Vault, AWS Secrets Manager, AWS KMS, GCP Secret
Manager, Kubernetes Secrets. `HostKey.ValueFrom` grows new `kind`
values that select a resolver.

Why: larger deployments mandate a specific secret store; today
`app/hostkey` only supports env + stored. A Resolver seam makes new
backends additive instead of a rewrite.

Size: ~3-4 days for the seam + one external backend (AWS Secrets
Manager first — most-requested). Each additional backend ~half day.

Where: `pkg/secret/` (new), `pkg/secret/aws/`, `pkg/secret/vault/`, …;
`app/hostkey` dispatches through Resolver; migration adds new
`kind` variants.

### 2. Batch processing (relay-native)

What: relay primitive for fire-and-forget bulk submissions, **working
for any upstream regardless of whether the provider exposes a batch
API**. Customer posts a batch, gets an ID, polls or receives a webhook
on completion, fetches results from S3 or relay storage. Worker pool
drains jobs through the existing `app/pipeline` (which already handles
retries / key selection / breakers). When the chosen Host's adapter
exposes a native batch API, pass through (50% discount); otherwise
simulate via concurrent pipeline calls.

Why: documented as the "third pillar" / infra-grade differentiator.
Relay-native batch means a customer can flip "run as batch" on any
model and get cost/throughput benefits even on providers that don't
ship a batch API.

Schema: `batches(id, policy_id, status, created_at, completed_at,
result_uri, error)` + `batch_items(batch_id, idx, input, output,
status, error)`. Customer API: `POST /v1/batches`,
`GET /v1/batches/{id}`, `POST /v1/batches/{id}/cancel`.

Size: ~2 weeks. Schema + API + worker + provider-batch dispatch +
result storage.

Where: `app/batch/` (new), `app/httpapi/inference/batches.go`, new
migrations, possibly `cmd/relay-batch-worker/` if the worker becomes
a separate binary.

Dependencies: lands cleaner once (3) exists so batch completion can
emit a webhook instead of forcing customer polling.

### 3. Webhooks per request-authoring unit

What: configurable outbound webhooks fired on terminal request /
batch events (completion, failure, rate-limit, breaker trip). Open
design question: **what level does the webhook attach to** — per
RelayKey, per Policy, per User, or all three with precedence rules.
Most likely answer: per RelayKey (finest grain that always exists),
falling back to per Policy (operator default), with per-User reserved
for SaaS-mode tenant defaults once B2 lands.

Schema: `webhooks(id, owner_kind, owner_id, url, events, secret_id,
status, created_at)` + delivery log. HMAC signing via stored secret.
Retry policy + DLQ.

Why: every async flow (batch first, long-running stream second) needs
push-style notification or customers eat polling cost. Also a
foundational SaaS feature.

Size: ~1 week. Includes a short design doc to pin the owner-kind
question before code.

Where: `app/webhook/` (new), `app/httpapi/control/webhooks.go`, new
migration. Delivery worker likely reuses the batch worker pool.

Dependencies: write the design doc first (`docs/webhooks.md`) to
resolve the owner-kind question.

### 4. Expanded adapter support

What: add new wire-shape adapters beyond `openai` and `anthropic`.
**Gemini native is done** (upstream-only, see Recently shipped). Remaining
candidates (rough order of demand): native inbound Gemini (URL-path model
extraction), AWS Bedrock Converse API, Cohere, Mistral native (currently
OpenAI-compatible so usable via `openai` adapter, but native unlocks
features).

Why: every new adapter widens the addressable upstream set. The adapter
seam is already in place — adding one is a contained slice.

Size: ~3-5 days per adapter (shape parser, Call, ExtractTokens,
streaming, tests).

Where: `pkg/adapters/<vendor>/` implementing `v1.Translator` + a `Spec`
literal in `cmd/relay/main.go` (see `pkg/adapters/gemini/` as the
reference for a shape with a URL-path model via `Spec.UpstreamPathFn`).

---

## Next — known follow-ups, not yet in flight

Not blocked, not prioritised above. Pick when one of the priority
queue items lands or when you want a parallel slice.

### SaaS path (multi-tenancy)

The order is fixed: B1 → B2 → B3 → B4. Each is a separate PR.

- **B1 — Users in Postgres + signup**. Replace YAML-backed
  `internal/identity`. Add `users` table, bcrypt passwords, signup
  endpoint, CLI bootstrap for the first admin. ~3-4 days. Where:
  `app/user/` (new package), migration, `app/httpapi/control/users.go`.
- **B2 — Org → Workspace → Project hierarchy**. Every catalog row
  gains an Org owner; session payload grows an `active_org_id`;
  RelayKey scopes to a Project. ~2 weeks. Where: `app/org`,
  `app/workspace`, `app/project`; backfill migration; snapshot
  restructuring.
- **B3 — Real authz behind `Authorizer`**. Casbin first; OpenFGA
  later only if/when fine-grained sharing matters. The handler-side
  seam (every CRUD/mutation routes through `d.Authz.Authorize`) is
  already in place. ~3 days. Where: `app/authz/casbin.go`, policy
  YAML/CSV in `config/authz/`.
- **B4 — JWT for programmatic control-API access**. A third caller
  type alongside cookies + relay keys: signed JWT for CLI/CI/customer
  scripts. Mint endpoint, revocation list, middleware. ~2 days.
  Where: `app/jwt/`, `app/httpapi/control/tokens.go`, new migration.

### Operator surface

- **UI re-mount**. Stage 5 deleted the legacy embedded console. Decide:
  re-mount the existing assets (if they're fit) or build a fresh UI
  against the typed OpenAPI spec. Size: ~1 day (re-mount) to ~2-4
  weeks (rewrite). Where: `app/httpapi/control/ui.go`, possibly a
  separate UI repo if rewriting.
- **Keypool admin observability**. `GET /host-keys/by-id/{id}/health`
  returning current breaker state (circuit, failure counter, cooldown
  deadline, last failure kind, last success time). ~half day. Where:
  `app/httpapi/control/host_keys.go` extension; `app/keypool` exposes
  a snapshot accessor.
- **Slug edit + referencing-rewriter**. Operators can't rename catalog
  rows today. Adding the UPDATE path requires walking every spec
  field that carries a slug ref. ~3 days. Where:
  `app/manifest/translate.go`, `app/httpapi/control/crud.go`.
- **CI/CD + deploy stack**. GitHub Actions workflow, Harbor push,
  Argo apply on merge to main. The Obsidian dev-workflow doc has the
  shape. ~1 day. Where: `.github/workflows/`, `Makefile`, `deploy/`.

### Cutover tech debt

- **A2 — Observability observers**. The lifecycle spine + usage emit
  shipped (see Recently shipped); remaining is adding observers on it:
  (a) a **ClickHouse usage sink** behind `app/usagelog`'s `Sink`
  interface — reference the deleted `pkg/eventlog/clickhouse.go` from git
  history; (b) **OTel tracing** — a span on the lifecycle `Context`,
  started at entry, ended in post-flight with routing attributes
  (replaces the deleted no-op `reqid` span); (c) **Prometheus** — wire
  `pkg/metrics` request counters/histograms +
  `relay_pipeline_post_flight_duration_seconds` onto the post-flight
  hook. Each is additive — register at boot, no pipeline change. ~1-2
  days each.
- **A2b — Per-request capture gaps**. The capture *fields* on the
  lifecycle event, researched against OpenRouter + LiteLLM. **Done:**
  (1) timing breakdown (`lifecycle.Timing` — anchor + upstream
  handoff/first-byte/done marks, µs offsets all anchored to start, never
  chained) giving TTFT + relay-overhead split, plus a `streamed` flag;
  (2) **failure events** — `pipeline.Run`/`proxy.Run` now fire a
  post-flight observer event on every error return via a defer, with a
  classified `ErrorKind` (`no_keys`, `keys_exhausted`, `upstream_error`
  +surfaced status, `rate_limited`, `client_canceled`, `timeout`, …) and
  `ErrorMessage`, so failed requests are no longer invisible to usage
  tracking. **Remaining:** (a) **routing-failure capture** — failures
  *before* `pipeline.Run` (model-not-found, no-host-binding, disabled
  policy) still emit nothing because the lifecycle Context is built at
  dispatch right before the runner; capturing them needs an earlier
  Context + a fire on the `mapRoutingErr` path. Lower volume than runner
  failures. (b) cheap fields the data's already on hand for —
  requested-model string, `finish_reason` (needs `v1.ExtractUsage` to
  also surface it), upstream attempt count, client-IP parity in pipeline
  mode. (c) **deliberately out of scope**: cost (derive in the sink from
  tokens × pricing, keep the event pricing-free), request/response
  content (S3 payload path), SaaS attribution (session/app/end-user —
  B-track).
- **A3 — Perf bench harness**. A `bench/pipeline/` harness against
  `app/pipeline.Pipeline.Run` **already exists** (and `bench/fakeanthropic`).
  Remaining: wire it into CI as a regression gate and document the
  baseline numbers. ~half day. (The "flying blind" framing was stale.)
- **A4 — Security leakage test** — DONE, and it found a real leak: normal
  -mode dispatch forwarded raw inbound headers to the upstream, so the
  relay key (`Authorization`/`X-Api-Key`) and `X-WR-*` control headers
  leaked to providers whose auth header isn't `Authorization`
  (Anthropic/Gemini). Fixed by stripping the denylist on a cloned header
  set in dispatch (mirrors the proxy path) + adding `X-Api-Key` to
  `StripDenylist`; regression test in `header_leakage_test.go`.
- **No-silent-drops adapter contract**. Codified as canonical-protocol
  **rule 11** (+ mirrored in CLAUDE.md): emit / carry in
  `provider_data`/`extensions` / annotate with a greppable
  `// canonical:` comment / surface safety-relevant signals — never
  accept-and-discard. **Remaining:** automated runtime `adapter_drop`
  warning emission, which needs a drop-sink threaded through the
  translator call signature (translators are pure, no logger today).
  ~1 day when picked up.

### Misc product features

- **MFA / password reset / SSO**. When SaaS launches with self-serve.
  Either DIY (TOTP, email link) or adopt SuperTokens / Kratos.
- **Per-org cluster isolation**. When a single tenant's traffic
  starts dominating, give them a dedicated pod group. Pure deployment
  story; no code changes.

---

## Backlog — needs design

Worth doing but not until the shape is settled. Drafting a design
doc in `docs/` is the first move for each.

### Mode-tier pricing (batch / priority / flex)

The shape: multiple Pricing rows per Model, distinguished by a
`metadata.labels.tier` (or a new `Pricing.Spec.Tier` field). Request-
side picker reads a header (`X-Relay-Tier: batch`) or infers from the
caller context (batch flow → batch tier automatically). Cost
computation reads the matched tier's rates.

Open design questions:
- Field on Pricing vs label on metadata vs separate join?
- How does a Policy declare which tiers it allows?
- What's the default-tier fallback rule?
- Does pricing-by-tier survive a re-import from LiteLLM?

Driven by: batch subsystem (Now #2) needs this to honour the 50%
discount of provider batch APIs. Could ship inline with batch or
right after — flag during design.

### Non-token pricing meters

Today's meters: 5 token sub-meters. Real LiteLLM coverage:
audio (per-second), images (per-image), video, web_search,
code_interpreter, file_search, character/pixel/query. Each is a new
`Meter` constant + a `Rate.Unit` value + a Cost() path that knows how
to count the dimension.

Open design questions:
- Where does the count come from when the upstream doesn't return it
  in the response? (Adapter pre-counts? Request-time inference?)
- How does a Model declare which meters apply? Today it's implicit
  per Pricing row.
- Does this need a new `Modality` → `Meter` mapping?

Driven by: when we actually proxy non-text shapes. Today every model
in seeded config is text-only.

---

## V2 — beyond the wedge

Bigger than a feature; its own product line. Off the v1 critical path —
finish the infra story (Now #1–4) before this competes for priority.

### Tool Gateway (capability parity layer)

A separate service that canonicalizes *tool* I/O the way Relay
canonicalizes *LLM wire* I/O — one tool contract, many backends (Brave /
Serper / SearXNG for search, a sandbox for code-exec, a guarded fetcher
for `web_fetch`), hosted or self-hosted, **in its own pods, never on the
hot path**. Goal: give Ollama and other non-frontier models the
server-side tools only frontier closed models ship today. Out-positions
OpenRouter on an axis they don't compete on. Full proposal — subsystem
boundary, canonical surface, SSRF threat model, open questions — in
`docs/v2-tool-gateway.md`.

## Icebox — deferred indefinitely

These were considered, found not to clear the bar, and parked. Touch
only if a concrete external signal flips the call.

### Cross-shape translation

`/v1/chat/completions` for a model whose binding declares
`adapter: anthropic`, with relay translating shapes via an OpenAI
canonical hub. Currently returns 400. Per CLAUDE.md: "same-format
passthrough is the 95% case." Unblock signal: real customer traffic
where wrong-shape requests are >5% of a meaningful tenant's load.

### Quota-aware key selection

CLAUDE.md aspirationally mentioned "weighted random by remaining
quota." Concretely: a per-key budget tracker, fed by usage observations,
that biases `Selector.Pick` toward keys with headroom. Existing algos
(prioritized / round-robin / LRU) already cover "drain key #1 first
then fall over." Adding quota-awareness adds complexity (observations
feedback loop) without a clear demand — operators reach for it when
they explicitly hit "cheap key first" and the prioritized algo isn't
expressive enough. Unblock signal: real operator request with a
specific scenario we can't model.

### Pluggable selection strategies

Plugin-style registry for custom selection algos (cost-tier-aware,
sticky-per-user, latency-aware). Today `KeySelection` is a closed
enum. The hardcoded set covers known use cases; adding plugin
infrastructure before there's a concrete second-party algo is over-
engineering. Unblock signal: a customer who can articulate "I want
this specific algo and we will fork to add it."

---

## How to pick

Default move: take the top item from **Now**. The order is chosen so
each unblocks the next without orphan work.

When in doubt about a Backlog item, write the design doc first
(`docs/<topic>.md`) — surfacing the open questions usually flips
"backlog" to "now" or "icebox."

Icebox items don't move without a specific external signal; don't
casually promote them.
