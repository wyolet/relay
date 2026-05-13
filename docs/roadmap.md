# Roadmap

The `app/` architecture cutover (PRs #110–#115) is done; E2E test
landed in #117. This doc tracks what comes next, in **priority order**.

Items list **what / why / rough size / where it lives**. When "where"
would take more than a sentence, the item is its own design doc
waiting to be written; the path under `docs/` is the placeholder.

---

## Now — priority queue

These three are next, in this order. Settings unblocks proxy
configuration plumbing; proxy is a thinner orchestrator that
benefits from Settings being shaped already; batch is the heaviest
and lands once the simpler flows are in.

### 1. Settings API (typed sectioned config)

What: `GET /settings/{section}` and `PUT /settings/{section}` per
known section (passthrough, limits, branding, …). Backed by a
generic `settings(key TEXT PK, value JSONB NOT NULL, updated_at
TIMESTAMPTZ)` table. Each section has a typed Go struct validated
server-side; the DB stores opaque JSON.

Why: Closes the `/passthrough` and `/attachments` removals from
stage 3 and gives a stable home for every future "global config
section." Pattern matches Stripe/GitHub/Linear. The "single-row
table per section" anti-pattern is what we chose not to do.

First section to ship: **passthrough** — the singleton that gates
proxy-mode anonymous traffic. Required input to (2) below.

Size: ~2 days. Migration + admin handler factory + one section.

Where: `app/settings/` (new package); `app/httpapi/control/settings.go`;
new migration; `cmd/relay/main.go` wiring.

### 2. Proxy mode — separate flow (`app/proxy`)

What: A second inference flow for "no relay key, caller brings their
own upstream key." Distinct package from `app/pipeline`, not a branch
in pipeline. Endpoint: same `/v1/chat/completions` and `/v1/messages`
shapes — the handler dispatches between pipeline (relay-key auth) and
proxy (passthrough auth) based on the inbound `Authorization` header
shape and the Settings.passthrough config.

Why: Captures a real customer segment — Claude Code, CLI dev tools,
and similar callers want "log my traffic, don't manage my keys."
Already a documented architecture seam ("anonymous mode is a separate
package, not a branch"); this just builds it.

Size: ~2-3 days. The proxy orchestrator is thin (no key-pool
selection, no per-key breaker; just rate-limit + stream-through).
Inference handlers grow a dispatch step.

Where: `app/proxy/` (new package); inference handlers grow a
dispatch fork; `Settings.Passthrough` from (1) gates it.

Dependencies: Settings (1) — at minimum the passthrough section
must exist so admins can enable/disable per-route or globally.

### 3. Batch subsystem

What: Relay primitive for fire-and-forget bulk submissions. Customer
posts a batch, gets a batch ID, polls or receives a webhook on
completion, fetches the results blob (from S3 or relay storage).
Worker pool drains jobs; uses provider batch APIs when available
(50% discount passthrough) and simulates via concurrent pipeline
calls otherwise.

Why: Differentiator. CLAUDE.md calls this the "third pillar" and the
infra-grade angle LiteLLM/OpenRouter can't match cleanly. Real demand
from teams running daily eval / dataset enrichment / nightly summary
jobs.

Sub-tasks:
- Schema: `batches(id, policy_id, status, created_at, completed_at,
  result_uri, error)` + `batch_items(batch_id, idx, input, output,
  status, error)`.
- Customer API: `POST /v1/batches`, `GET /v1/batches/{id}`,
  `POST /v1/batches/{id}/cancel`, webhook on completion.
- Worker: long-running goroutine that pulls pending items, runs them
  through the existing app/pipeline (which already orchestrates
  retries, key selection, etc.), writes results.
- Provider batch passthrough: detect when the chosen Host's adapter
  exposes a batch API (OpenAI does; Anthropic does); pass through
  if so. Otherwise simulate.
- Storage: results to S3 (opt-in) or to a `batch_results` table for
  small batches.

Size: ~2 weeks. Schema + API + worker + provider-batch dispatch +
result storage + webhook signing.

Where: `app/batch/` (new package); `app/httpapi/inference/batches.go`;
new migrations; possibly `cmd/relay-batch-worker/` if we want the
worker as a separate binary.

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

Driven by: batch subsystem (priority 3) needs this to honour the 50%
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
