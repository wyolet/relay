# Roadmap — OSS core (Phase A)

**Goal:** open-source the infra-grade core and reach a credible public
launch. The wedge is the infrastructure axis — performance, key pooling,
batch orchestration, observability — under a BYO-key / no-reseller model.
Feature parity with LiteLLM is *not* the goal; infra-grade throughput is.

This doc has two halves:

- **Part 1 — Core infra features.** The engineering that makes the open
  core worth running. These are the differentiators; ship them so the
  public release has teeth, not just a license file.
- **Part 2 — Launch readiness.** The work to make the repo publishable —
  license, history scrub, docs, CI, community surface.

Order is deliberate: land the headline infra features (batch, webhooks,
adapters, observability completion) *while* the launch-readiness work
proceeds in parallel, then cut the public release once both halves are
green. Don't open-source a core that's missing its headline feature.

See [`roadmap.md`](roadmap.md) for shared "Recently shipped" history.

---

## Part 1 — Core infra features

### A1. Batch processing (relay-native) — headline differentiator

**What:** a relay primitive for fire-and-forget bulk submissions,
**working for any upstream regardless of whether the provider exposes a
batch API**. Customer posts a batch, gets an ID, polls or receives a
webhook on completion (see A2), fetches results from S3 or relay storage.
A worker pool drains jobs through the existing `app/pipeline` (which
already handles retries / key selection / breakers). When the chosen
Host's adapter exposes a native batch API, pass through for the 50%
discount; otherwise simulate via concurrent pipeline calls.

**Why:** documented as the "third pillar" / infra-grade differentiator.
Relay-native batch means a customer can flip "run as batch" on *any*
model and get cost/throughput benefits even on providers that don't ship
a batch API. This is the heaviest build and the headline feature — it
should land before the public launch.

**Schema:** `batches(id, policy_id, status, created_at, completed_at,
result_uri, error)` + `batch_items(batch_id, idx, input, output, status,
error)`.

**Customer API:** `POST /v1/batches`, `GET /v1/batches/{id}`,
`POST /v1/batches/{id}/cancel`.

**Size:** ~2 weeks. Schema + API + worker + provider-batch dispatch +
result storage.

**Where:** `app/batch/` (new), `app/httpapi/inference/batches.go`, new
migrations, possibly `cmd/relay-batch-worker/` if the worker becomes a
separate binary.

**Dependencies:** lands cleaner once A2 (webhooks) exists so batch
completion can emit a webhook instead of forcing customer polling.
Pairs with mode-tier pricing (SaaS `roadmap-saas.md`) to honour the
provider-batch 50% discount — but batch ships first; pricing is additive.

### A2. Webhooks per request-authoring unit

**What:** configurable outbound webhooks fired on terminal request /
batch events (completion, failure, rate-limit, breaker trip).

**Open design question:** **what level does the webhook attach to** — per
RelayKey, per Policy, per User, or all three with precedence rules? Most
likely answer: per RelayKey (finest grain that always exists), falling
back to per Policy (operator default), with per-User reserved for SaaS
tenant defaults once multi-tenancy lands. **Write `docs/webhooks.md`
first** to pin this before code.

**Why:** every async flow (batch first, long-running stream second) needs
push-style notification or customers eat polling cost. Also a foundational
SaaS feature.

**Schema:** `webhooks(id, owner_kind, owner_id, url, events, secret_id,
status, created_at)` + delivery log. HMAC signing via stored secret
(reuses `pkg/secret`). Retry policy + DLQ.

**Size:** ~1 week including the design doc.

**Where:** `app/webhook/` (new), `app/httpapi/control/webhooks.go`, new
migration. Delivery worker likely reuses the batch worker pool.

### A3. Expanded adapter support

**What:** add new wire-shape adapters beyond `openai` and `anthropic`.
Gemini native is **done** (upstream-only). Remaining candidates, in rough
order of demand:

1. **Native inbound Gemini** — needs URL-path `{model}` extraction on the
   inbound route (the upstream side already exists via `Spec.UpstreamPathFn`).
2. **AWS Bedrock Converse API** — unlocks the unified Bedrock surface.
3. **Cohere** native.
4. **Mistral** native — currently usable via the `openai` adapter
   (OpenAI-compatible), but native unlocks Mistral-specific features.

**Why:** every new adapter widens the addressable upstream set. The
adapter seam is already in place — adding one is a contained slice.

**Size:** ~3–5 days per adapter (shape parser, `Call`, `ExtractTokens`,
streaming, tests).

**Where:** `sdk/adapters/<vendor>/` implementing `v1.Translator` + a `Spec`
literal in `cmd/relay/main.go`. `sdk/adapters/gemini/` is the reference
for a URL-path-model shape.

### A4. Observability completion

The lifecycle spine + usage emit shipped; the ClickHouse/Postgres/valkey
usage sinks shipped. Remaining observers (each is additive — register at
boot, no pipeline change):

- **A4a — OTel tracing.** A span on the lifecycle `Context`, started at
  entry, ended in post-flight with routing attributes. Replaces the
  deleted no-op `reqid` span. ~1–2 days.
- **A4b — Prometheus.** Wire `pkg/metrics` request counters/histograms +
  `relay_pipeline_post_flight_duration_seconds` onto the post-flight hook.
  ~1–2 days.

**Why:** "observability" is one of the four wedge axes. The OSS core needs
real traces + metrics out of the box; the enterprise track
(`roadmap-enterprise.md`) then *wires and hardens* these into a supported
deployment story — but the code lands here.

### A5. Per-request capture gaps (A2b carryover)

The capture *fields* on the lifecycle event. Most shipped (timing
breakdown, failure events with classified `ErrorKind`, unified Context at
entry + routing-failure capture, `finish_reason`/`requested_model`/`attempts`
enrichment, echo-usage, payload content capture). **Remaining:**

- (b) Pure server-misconfig 500 guards (no spec/adapter/translator
  registered) don't fire an event — they signal a boot-config bug, not
  request telemetry. Decide whether to surface as a distinct event class.
- (c) Per-shape parse failures *before* `Dispatch` (malformed body that
  can't yield a model name) live at the route edge, before the Context is
  minted. Needs an edge-level capture hook if we want them visible.

**Out of scope by design:** cost (derive in the sink from tokens ×
pricing, keep the event pricing-free); SaaS attribution
(session/app/end-user — that's `roadmap-saas.md`).

### A6. No-silent-drops adapter contract — automation

Codified as canonical-protocol **rule 11** (emit / carry in
`provider_data`/`extensions` / annotate with a greppable `// canonical:`
comment / surface safety-relevant signals — never accept-and-discard).
**Remaining:** automated runtime `adapter_drop` warning emission, which
needs a drop-sink threaded through the translator call signature
(translators are pure, no logger today). ~1 day when picked up.

### A7. Perf bench harness as CI gate (A3 carryover)

A `bench/pipeline/` harness against `app/pipeline.Pipeline.Run` already
exists (plus `bench/fakeanthropic`). **Remaining:** wire it into CI as a
regression gate and document the baseline numbers against the performance
contract (in-process: p50 < 100µs, p99 < 500µs; live: p50 < 2ms, p99 10ms
SLO / 15ms public claim). ~half day. The "flying blind" framing is stale —
the harness exists, it just isn't gating.

### A8. Media offload — provider-side file references

**What:** for large media (images/audio/video/docs), upload once to the
chosen host's **own** file API and reference by `file_id` on resends —
turning O(N) re-uploads of the same blob across a multi-turn session into
O(1). Media-only (text stays inline), opt-in per host capability, never a
silent strip.

**Design (settled):** rejected the relay-hosted-URL variant (privacy /
availability-inversion / no uniform provider mechanism). Cache is
`(credential-scope, content-hash) → file_id` in kv (TTL ≤ provider
retention); a stale/expired/cross-scope ref 404s pre-first-byte and
**rides the existing KeyAgent self-heal loop** (re-upload → rewrite →
retry). Pre-flight pipeline stage gated by a `Spec` capability —
translators stay pure (rule 6). Shares the content-hash primitive with the
storage-side content-addressed blob store. Full design:
`docs/media-offload.md`.

**Size:** ~1 week.

### A9. Real-time log streaming (live tail)

**What:** a live tail of the lifecycle event stream. The CH query store is
batched (~10s flush) — history, not a live feed. Live tail is an
*additional consumer* of the event stream, not a schema change. Three
tiers by cost:

- (a) **Live tail of completed events** — cheap; a real-time fanout
  observer (Redis stream / SSE) next to the batch sink, re-broadcast by the
  control plane.
- (b) **In-flight visibility** (watch a request mid-stream) — needs a
  separate in-flight/span registry since the log event only finalizes at
  post-flight.
- (c) CH-for-real-time — explicitly out of scope.

WS already produces a per-request log event per frame; the only capture
wrinkle is accumulating response frames per correlation-id. Full notes:
`docs/payload-logging.md` "Real-time log streaming". Ties into the unified
usage/logs model.

### A10. Non-token pricing meters

**What:** today's meters are 5 token sub-meters. Real coverage needs
audio (per-second), images (per-image), video, web_search,
code_interpreter, file_search, character/pixel/query. Each is a new
`Meter` constant + a `Rate.Unit` value + a `Cost()` path that counts the
dimension.

**Open design questions:**
- Where does the count come from when the upstream doesn't return it in
  the response? (Adapter pre-counts? Request-time inference?)
- How does a Model declare which meters apply? Today it's implicit per
  Pricing row.
- Does this need a new `Modality` → `Meter` mapping?

**Driven by:** when we actually proxy non-text shapes. Today every seeded
model is text-only. Design-doc-first. (Mode-tier pricing — batch/priority/
flex tiers — is a billing concern and lives in `roadmap-saas.md`, though
it shares the multi-row-Pricing shape with this.)

### A21. List filtering — control-plane query contract

**Shipped:** the `pkg/filter` engine + config-list wiring + Metadata
timestamps (#262–#264) — policies/models/hosts/relay-keys filter, sort,
and window server-side with `total` and 400-on-typo.

**Open follow-ups** (each its own PR, see [`filtering.md`](filtering.md)):
- **F1. Model capability filter** (`?capability=`) — standalone, do first;
  needs an AND-membership decision in `pkg/filter` (`MatchAll`). Size S.
- **F2.** Remaining allowlists (provider_id/modality/dates on models;
  host-keys `value_kind`; providers/pricing schemas; uniform `label=k=v`).
- **F3.** Host-key breaker-state filter (`?health=`) — snapshot+kv join,
  design-first.
- **F4.** Logs/Usage filter gap-fill (`status_class`, `ttft_ms`,
  `has_payload`, `host_key_id`, `q`, `sort`, …) — highest UI impact.

**Driven by:** the relay-ui filter convention; F4 deletes the most mock
data from the live dashboard.

---

## Part 2 — Launch readiness

The path from private repo to credible public release.

### A11. Open-core line + license

- **Decide the open-core line** — what's OSS vs held back as commercial
  (enterprise/SaaS features from the other tracks). Write it down; it
  drives every later "is this public?" call.
- **Pick a license** — permissive Apache-2.0 vs protective BSL/AGPL to
  stop cloud resale. The no-reseller wedge argues for a protective
  license; decide explicitly.

### A12. Pre-publication audit

- **Scrub git history** for leaked secrets/keys/.env before going public.
  Full-history audit, not just HEAD.
- **Decouple from private infra** — replace Obsidian-vault / dev-stack
  references with self-contained public docs. (CLAUDE.md and roadmap docs
  currently point at `~/Documents/Obsidian Vault/...` and an internal
  dev-stack; those can't ship.)

### A13. Repo hygiene files

- `LICENSE`, `NOTICE`, per-file license headers.
- `SECURITY.md`, `CODE_OF_CONDUCT.md`.
- `CONTRIBUTING.md` + issue/PR templates + contribution workflow.

### A14. Public-facing docs + README

- **Rewrite README for the public** — positioning vs OpenRouter/LiteLLM,
  the BYO-key / no-reseller wedge.
- **Public docs:** architecture, canonical-protocol, deploy guide, config
  reference. Much exists in `docs/` already but assumes internal context —
  needs a public pass.

### A15. One-command self-host quickstart

`docker compose up` → a working relay. The single most important
first-impression artifact. Lives off `deploy/compose/`.

### A16. Public catalog story

Decide: open the `wyolet/relay-catalog` repo vs ship baked-in defaults
only. The `go:embed`'d default already exists for airgapped/first-boot;
the question is whether the curated catalog repo is public + how releases
are tagged (`RELAY_CATALOG_VERSION`).

### A17. Public CI + release pipeline

- Public CI: build / test / lint / race + the canonical-protocol grep
  tests (rules 1, 2, 4, 10) that must hold on every commit.
- Release pipeline: tagged binaries + ghcr.io images.
- Semver + changelog + release process.

### A18. Starter deploy manifests

Starter k8s / Helm manifests for self-hosters (the enterprise track owns
the *hardened* Helm chart with TLS/HA; this is the get-started version).

### A19. Telemetry + community

- **Opt-in anonymous telemetry** decision + privacy stance. Default-off,
  clearly documented.
- **Community surface** — Discussions / Discord — where inbound lands.

---

## Dependencies out of this track

- Enterprise (`roadmap-enterprise.md`) reuses A4 (observability) as its
  "observability wired" deliverable and A7 (bench) as its perf SLO gate.
- SaaS (`roadmap-saas.md`) reuses A1 (batch) + A2 (webhooks) as async
  primitives and A10's multi-row-Pricing shape for mode-tier billing.
