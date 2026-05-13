# Post-cutover roadmap

The `app/` architecture cutover (six staged PRs, #110–#115) is done.
The codebase is now organised around the `app/` packages; the legacy
tree is deleted. This doc tracks what comes next, grouped by readiness
rather than priority — pick tracks based on what unblocks the most
downstream work.

Items list **what / why / rough size / where it lives**. If a row would
take more than a sentence of "where," it's its own design doc waiting
to be written; the path under `docs/` is the placeholder for that.

---

## Track A — Close out the cutover

Small follow-ups that finish the rewrite story. None of them are
blockers for the existing PRs but they close gaps we left open.

### A1. Stage 6 — E2E compose test against mock upstream

What: Bring the test pg compose up with a `mock-upstream` service and
a `relay` service. Integration test (`integration` build tag) drives
the admin API to wire a Host/HostKey/Policy/RelayKey, then issues a
`/v1/chat/completions` and asserts the upstream was hit with the right
URL + auth and the response streamed back intact.

Why: Today every layer has unit/orchestration tests but nothing
exercises the full path end-to-end. This is where adapter dispatch,
header forwarding, NOTIFY propagation, and key-pool selection get
proved on a real wire.

Size: ~2 days. Mock upstream is ~100 LOC; compose plumbing + test
harness ~200; integration test ~300.

Where: `integration/e2e_test.go` (new); `deploy/compose/docker-compose.test.yml`
(extend); `cmd/mock-upstream/main.go` (new — or in `bench/fakeanthropic`
which already has the right shape).

### A2. Observability emit rebuild

What: Wire `app/pipeline.OnSuccess` to OTel spans + the eventlog
writer. Currently `pipeline.OnSuccess` exists and fires, but no caller
actually emits anything. `internal/usage` still has the scaffolding
(`Lifecycle`, `Record`, `metrics`) but isn't wired into a hot path and
was stripped of its catalog-coupled cost stamping in stage 5.

Why: Without this, we have no usage records, no cost attribution, no
per-request traces. CLAUDE.md's claims about ClickHouse analytics +
OTel spans are aspirational until this lands.

Sub-tasks:
- Define an `app/usage` package with a clean `Tokens` + `Lifecycle`
  shape that doesn't carry catalog types.
- Wire a callback from the inference handler that builds a Lifecycle
  and `Record()`s it on `OnSuccess`.
- Replace cost stamping with a lookup against `app/pricing` via the
  catalog snapshot. The shape is already in
  `app/pricing.Pricing.RateFor(meter, tokens)` — pricing math just
  needs to be re-attached.
- Move `ParseMetadataHeader`, `SpanName`, `ContextWithSpan` out of
  `internal/usage` into `pkg/usage` or `app/usage` so `internal/usage`
  can be retired entirely.

Size: ~3 days. Mostly mechanical once the new package shape lands.

Where: `app/usage/` (new package); `app/httpapi/inference/chat.go` +
`messages.go` (wire the callback); `internal/usage` (eventually
delete).

### A3. Performance bench harness

What: New `bench/` harness that targets `app/pipeline.Pipeline.Run`
in-process with `kv.Mem` to validate the perf contract in CLAUDE.md
(p50 < 100µs, p99 < 500µs in-bench).

Why: We deleted `bench/bench_test.go` in stage 5; we're flying blind
on regressions. CLAUDE.md says "load-test on every PR; fail builds on
p99 regression" — that promise is currently unhonoured.

Size: ~1 day. The fakeanthropic upstream already exists; mostly
plumbing.

Where: `bench/bench_test.go` (new).

### A4. Security leakage test port

What: Re-point the deleted `pkg/httpheader/leakage_test.go` at the new
`app/api/openai` + `app/api/anthropic` adapters. Asserts that
sensitive inbound headers don't leak to upstream when adapters call
out.

Why: Real security regression we removed in stage 5; small to
restore.

Size: ~half a day.

Where: `app/api/openai/leakage_test.go`, `app/api/anthropic/leakage_test.go`.

### A5. Settings API

What: Typed sectioned settings — `GET /settings/{section}` and `PUT
/settings/{section}` per known section (passthrough, limits, branding,
…). Backed by a generic `settings(key TEXT PK, value JSONB NOT NULL,
updated_at TIMESTAMPTZ)` table. Each section has a typed Go struct
validated server-side; the DB stores opaque JSON.

Why: Closes the `/passthrough` and `/attachments` removals from stage
3. Right shape long-term (Stripe/GitHub/Linear pattern). The "single
row table per section" anti-pattern is what we chose not to do.

Size: ~2 days. Table + migration + admin handler factory + one
section (passthrough) as the first ported one.

Where: `app/settings/` (new package); `app/httpapi/control/settings.go`;
new migration.

---

## Track B — Product gaps before v1 launchable

These are the gaps between "great internal infra" and "something a
real customer can use." Order them by what your launch story
requires.

### B1. User table in Postgres + signup flow

What: Replace YAML-backed `internal/identity` with a Postgres `users`
table; expose `/users` CRUD; add a signup endpoint that bcrypts a
password and creates the row. CLI bootstrap (`relay user create`) for
the first admin since you can't signup-then-login on a fresh DB.

Why: YAML-only identity blocks any SaaS-style onboarding. Editing
files + restarting is fine for one operator; not fine for paying
customers.

Size: ~3–4 days. Mostly mirrors the existing `app/X.Store` pattern.
Tricky bits: bootstrap (can't authenticate without an admin user), and
the move-from-YAML migration story for existing operators.

Where: `app/user/` (new package, mirrors the eight catalog entities);
`internal/identity` (eventually delete); migration; bootstrap CLI;
`app/httpapi/control/users.go`.

### B2. Multi-tenancy: Org → Workspace → Project

What: Add the org-scoped hierarchy from CLAUDE.md's domain model.
Every catalog row gains an Org owner. Resolution adds an "active org"
to the session. RelayKey scopes to a Project. Quota/budget hangs off
Org or Project.

Why: This is the SaaS-ification step. Without it, the relay serves
one tenant at a time.

Size: ~2 weeks. Schema changes, migration with backfill, snapshot
restructuring (org-indexed lookups), session payload growth, admin
surface for org switching.

Dependencies: B1 (users must exist before they can belong to orgs).

Where: `app/org/`, `app/workspace/`, `app/project/` (new packages);
new migrations; `app/actor.Actor.ActiveOrgID` field (already reserved
in the v1 shape); `app/authz` impl beyond AlwaysAllowAuthenticated.

### B3. Real authz behind the Authorizer interface

What: Replace `authz.AlwaysAllowAuthenticated` with a real
implementation. Casbin first (embedded; in-process; works against the
v1 model of "admin user can do anything in their org"). OpenFGA / Keto
later only if/when fine-grained sharing (Drive-style) becomes a
requirement.

Why: B2 only matters if we actually enforce. The handler-side seam
(every CRUD/mutation routes through `d.Authz.Authorize`) is already in
place from stage 3, so this is an implementation swap — no handler
changes.

Size: ~3 days for Casbin. Includes a small policy file shipped in
repo defining the v1 roles (org admin, member, viewer).

Dependencies: B2 (orgs need to exist for policies to scope to them).

Where: `app/authz/casbin.go` (new); a policy YAML/CSV checked into
`config/authz/`.

### B4. JWT for programmatic control-API access

What: A third caller type alongside session cookies and relay keys:
JWT signed by relay, used by CLI tools / CI / customer scripts that
need user-scoped control API access. Issuance endpoint (`POST
/auth/tokens`) for users to mint, list, revoke.

Why: Browser session cookies are wrong for headless tools; relay keys
are scoped to inference, not control. JWT closes the gap.

Size: ~2 days. JWT lib + the issuance/revocation table + a middleware
that checks `Authorization: Bearer eyJ...`.

Dependencies: B1 (per-user tokens need real users).

Where: `app/jwt/` (new); `app/httpapi/control/tokens.go`; new
migration for the issued-token revocation list.

### B5. UI re-mount

What: The legacy embedded a console at `cmd/relay/_legacy/ui.go` +
`cmd/relay/web/dist/`. Stage 5 deleted the Go mount; the static
assets may or may not still be there. Either re-mount the legacy UI
or build a new one against the typed OpenAPI (which is now actually
honest).

Why: Dev experience + customer-facing console. The fully-typed huma
specs in stages 3+4 are the right input for a typed TS client (e.g.
generated via `openapi-typescript-codegen` or `orval`).

Size: depends entirely on whether the legacy UI is fit to keep or
needs a rewrite. If keep: ~1 day to re-mount + wire to the new admin
API. If rewrite: ~2–4 weeks of frontend work.

Where: `app/httpapi/control/ui.go` (new mount); `cmd/relay/web/`
(check what's left); separate UI repo if rewriting.

---

## Track C — Operational maturity

Things that don't block launch but matter for the operator experience
once real traffic is on the relay.

### C1. Anonymous / passthrough flow (`app/proxy`)

What: Separate package for the "no relay key, caller brings their own
upstream key" flow. Stage 4 left this out intentionally — pipeline is
authenticated-only. `app/proxy` would have its own thin orchestrator
(no key-pool selection; just stream-through with rate limit
enforcement).

Why: Customers who want zero-config passthrough for testing or
dev-tier traffic. Some inference use cases (Claude Code routing
through relay for observability without touching the customer's
upstream key) want exactly this.

Size: ~2 days.

Where: `app/proxy/` (new); `app/httpapi/inference/` (alternate route
or header-driven dispatch within `/v1/*`).

### C2. Keypool admin observability

What: `GET /host-keys/by-id/{id}/health` returning the current
breaker state: circuit (closed/open/half-open), failure counter,
cooldown deadline, last failure kind, last success time. Today this
is opaque — operator can't answer "why isn't key X being used?"
without `kubectl exec` into Redis.

Why: First thing an operator wants when a request fails.

Size: ~half a day. Keypool already has the state; this is just an
admin handler.

Where: `app/httpapi/control/host_keys.go` (extend); maybe
`app/keypool.SnapshotKeyState(keyHash)` accessor.

### C3. Quota-aware key selection

What: CLAUDE.md promises "weighted random by remaining quota."
Current `KeySelection` enum: prioritized, round-robin, LRU. Need to
either verify weighted-by-quota exists or add it.

Why: Without it, you can't say "drain key X first because it has
budget" — you only get "use key #1 until it fails."

Size: ~2 days. Need a quota-tracking pass per key (which means tying
into rate-limit observations); then the selection algo.

Dependencies: A2 (observations stream needs to exist).

Where: `app/keypool/keypool.go` — new `KeySelectionWeighted` constant
+ corresponding Lua path.

### C4. Pluggable selection strategies

What: Today's `KeySelection` is a hardcoded enum. Replace with a
strategy registry so operators (or third parties) can add their own
without forking. e.g. sticky-session-per-user, latency-aware,
cost-tier-aware.

Why: Optional. Useful for advanced customers; not on the launch path.

Size: ~2 days. Mostly refactor.

Where: `app/keypool/strategy.go` (new).

### C5. Slug edit + referencing-rewriter

What: Today, slug renames aren't implemented — `Metadata.Name` is
mutable in the API contract but no UPDATE path exists. When we add it,
we need a referencing-rewriter that walks every spec field carrying a
slug ref (config-side; the DB stores ids) and updates them.

Why: Operators expect to rename things. Leaving this unfixed creates
"can't undo a typo" tickets.

Size: ~3 days, mostly because of the rewriter (every place where a
slug ref lives needs to be updated atomically).

Where: `app/manifest/translate.go` (rewriter); `app/httpapi/control/crud.go`
(PUT path handles slug edit).

### C6. CI/CD + deploy stack updates

What: GitHub Actions workflow against the new arch; Harbor image push;
Argo apply on merge to main. Verify the existing `make dev` stack
still bakes the right image.

Why: We've been merging directly without CI signals.

Size: ~1 day to wire up. The CLAUDE.md dev-workflow doc has the
existing pipeline shape; we just need to confirm it still applies.

Where: `.github/workflows/` (new or update); `Makefile`; `deploy/`.

---

## Track D — Future / when traffic demands

Items consciously deferred until evidence justifies them. Capturing
here so we don't reinvent the analysis.

- **Cross-shape translation** — caller hits `/v1/chat/completions` for
  a model whose binding declares `adapter: anthropic`. Currently 400.
  When 5% of real requests hit this case, build pairwise transforms
  via OpenAI as the canonical hub (per CLAUDE.md). Lossiness only
  becomes a problem with the OpenAI-hub model if traffic proves it.
- **MFA / password reset / SSO** — when SaaS launches with self-serve.
  Either DIY (TOTP, email link) or adopt SuperTokens / Kratos.
- **Batch subsystem** — relay-side batch primitive that uses provider
  batch APIs when available (50% discount passthrough) and simulates
  via worker pool otherwise. CLAUDE.md has the design.
- **ClickHouse persistence + analytics surface** — usage records to
  ClickHouse via the eventlog tier (already in
  `pkg/eventlog/BackendClickHouse`); query surface for dashboards.
  Depends on A2 actually emitting things.
- **Per-org cluster isolation** — when a single tenant's traffic
  starts dominating, give them a dedicated pod group. Pure deployment
  story; no code changes.

---

## How to pick what's next

A1–A5 close the rewrite story; each is ~1–3 days and orthogonal.
B1–B5 unlock launch; they have ordering (users → orgs → authz; users
→ JWT). C1–C6 raise the ops floor; pick what your first real customer
will complain about. D items wait for evidence.

If launch is the goal: **B1 → B2 → B3 → A2 → A1 → B5** is a path that
gets you to a usable v1 with real auth, real users, and real
observability. ~6 weeks if executed in order; can parallelise A
items.

If "make the rewrite ship-shape first" is the goal: **A1 → A2 → A4 →
A3 → A5** in order; ~10 days.

Either way, the cutover itself is done — every item above is a fresh
slice, not legacy debt.
