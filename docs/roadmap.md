# Roadmap

The `app/` architecture cutover (PRs #110–#115) is done; E2E test
landed in #117. This doc tracks what comes next, in **priority order**.

Items list **what / why / rough size / where it lives**. When "where"
would take more than a sentence, the item is its own design doc
waiting to be written; the path under `docs/` is the placeholder.

---

## Now — priority queue

Five items, in this order. Order is intentional: secrets unblocks every
deployment that mandates a non-env secret store; batch is the heaviest
build and the headline differentiator; webhooks unlock async UX once
batch lands; new transports broaden the wire surface; new adapters
broaden the upstream surface.

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

### 4. WebSocket transport

What: second customer-facing transport after HTTP. `/v1/ws` (and a
shape-matching anthropic endpoint) accepting framed requests over a
single long-lived connection; multiplexed responses streamed back on
the same socket. Same pipeline underneath; the transport layer
translates frame ↔ `pipeline.Request` / `pipeline.Result`.

Why: meaningful latency win for chatty clients (IDE / agent loops)
that pay TLS handshake + HTTP overhead per turn. Also matches what
some upstreams (OpenAI Realtime, Anthropic streaming) increasingly
expose.

Size: ~1 week. Frame protocol + handler + reuse of the existing
adapter/pipeline stack.

Where: `app/httpapi/inference/ws.go` + `app/transport/ws/` (or
similar). `app/pipeline` stays untouched — transport is a thin
shell around it.

### 5. Expanded adapter support

What: add new wire-shape adapters beyond `openai` and `anthropic`.
Concrete candidates (rough order of demand): Google Gemini native,
Cohere, Mistral native (currently OpenAI-compatible so usable via
`openai` adapter, but native unlocks features), AWS Bedrock Converse
API.

Why: every new adapter widens the addressable upstream set. The
adapter seam (`app/adapter.Kind` + `app/api/<kind>` per shape) is
already in place — adding one is a contained slice.

Size: ~3-5 days per adapter (shape parser, Call, ExtractTokens,
streaming, tests).

Where: `pkg/api/<kind>/` (pure shape, vendorable) +
`app/api/<kind>/` (relay-side glue); register new `Kind` in
`app/adapter`.

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

- **A2 — Observability emit rebuild**. Wire `pipeline.OnSuccess` to
  OTel + eventlog + pricing-cost stamping. Currently the callback
  fires but nothing emits. ~3 days. **Explicitly deferred** for now;
  pick up once feature work stabilises.
- **A3 — Perf bench harness**. New `bench/` against
  `app/pipeline.Pipeline.Run`. We're flying blind on regressions
  until this lands. ~1 day.
- **A4 — Security leakage test**. Re-point the deleted
  `pkg/httpheader/leakage_test.go` at the new `app/api/*` adapters.
  ~half day.

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
