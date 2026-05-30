# Roadmap — Enterprise / on-prem (Phase B)

**Goal:** the hardened build a single organisation runs on its own
infrastructure. The OSS core (`roadmap-oss.md`) is the engine; this track
makes it *operable, auditable, and secure* for a customer who needs SSO,
real authz, audit trails, HA, air-gap, and a license/entitlement story.

This is **not** multi-tenancy — that's SaaS (`roadmap-saas.md`). Here
there's one org, possibly with light internal team/project scoping. The
identity, authz, and org-scoping seams built here are deliberately the
foundation the SaaS track extends.

**Ordering:** the auth/identity chain is fixed — B1 → B2 → B3 → B4 — each
a separate PR, because every later item assumes Postgres-backed identity.
The operator-surface and hardening items can interleave once B1 lands.

The handler-side authz seam already exists: every CRUD/mutation routes
through `d.Authz.Authorize(ctx, action, resource)` (v1 impl
`AlwaysAllowAuthenticated`). `app/actor.Actor` already carries `UserID`,
`Username`, `SessionID`, `AdminToken`, with reserved `ActiveOrgID` +
`Roles` slots. **Do not branch handlers on identity directly — always go
through the Authorizer.**

---

## Identity & access chain (B1 → B4, fixed order)

### B1. Users in Postgres + signup

**What:** replace the YAML-backed `internal/identity`. Add a `users`
table, bcrypt passwords (the verifier already handles `$2a/$2b/$2y` with a
plain-compare deprecation fallback), a signup endpoint, and a CLI
bootstrap for the first admin.

**Why:** YAML identity doesn't scale past a handful of operators and can't
support password reset, lockout, or audit. Everything downstream (authz,
audit, SSO) assumes a real user store.

**Size:** ~3–4 days.

**Where:** `app/user/` (new package), migration,
`app/httpapi/control/users.go`.

### B2. Hardened authN

**What:** password policy (length/complexity/rotation), session lifecycle
(idle + absolute timeout, server-side destroy on logout — sessions are
already real scs-over-kv, opaque tokens rotated on login), login
throttling / lockout, and audit of auth events (feeds B5).

**Why:** baseline for any security review. The session machinery exists;
this hardens the policy around it.

**Size:** ~3 days.

**Where:** `app/session`, `internal/identity` → `app/user`,
`app/httpapi/control/auth.go`.

### B3. Real authz behind `Authorizer`

**What:** implement the `Authorizer` with roles. Casbin first; OpenFGA
later only if/when fine-grained sharing matters. The handler seam is
already in place — this is an implementation swap, not a handler rewrite.

**Why:** "every authenticated caller can do everything"
(`AlwaysAllowAuthenticated`) is fine for a single operator, not for an
org with separated duties.

**Size:** ~3 days.

**Where:** `app/authz/casbin.go`, policy YAML/CSV in `config/authz/`.

### B4. JWT for programmatic control-API access

**What:** a third caller type alongside cookies + relay keys: signed JWT
for CLI/CI/customer scripts hitting the control API. Mint endpoint,
revocation list, middleware.

**Why:** operators automating the control plane shouldn't reuse the
break-glass `RELAY_ADMIN_TOKEN` or scrape session cookies.

**Size:** ~2 days.

**Where:** `app/jwt/`, `app/httpapi/control/tokens.go`, new migration.

### B5. SSO / OIDC + SAML on the console

**What:** OIDC + SAML on the control console, reusing the workspace
pattern. Maps external identities onto B1 users + B3 roles.

**Why:** table-stakes for enterprise procurement; nobody provisions local
passwords for an internal tool.

**Size:** ~1–2 weeks (two protocols, identity mapping, JIT provisioning).

**Where:** `app/sso/` (new), `app/httpapi/control/auth.go`, `app/user`
linkage. Consider SuperTokens / Kratos vs DIY — decide in a design doc.

### B6. Audit logging

**What:** who-did-what on the control plane — every CRUD/mutation +
auth event, queryable + exportable. Distinct from the *request* log
(that's the usage/payload observers); this is the *administrative* log.

**Why:** SOC2 + incident forensics require it. Routes naturally through
the same Authorizer call site that already wraps every mutation.

**Size:** ~3–4 days.

**Where:** `app/audit/` (new) emitting at the Authorizer boundary, a
`audit_log` table, `GET /audit` read API.

### B7. Light team/project scoping (single org)

**What:** internal team/project scoping *within* one org — not the full
Org→Workspace→Project hierarchy (that's SaaS B-track in
`roadmap-saas.md`). Enough to scope RelayKeys and usage views to an
internal team. Reuses the `ActiveOrgID`/`Roles` actor slots.

**Why:** a single enterprise still wants to separate "the data-science
team's keys" from "the platform team's." This is the *seam* the SaaS
multi-tenancy build later widens — do it light here, full there.

**Size:** ~1 week.

**Where:** `app/project/` (light), RelayKey scoping, snapshot filter.

---

## Operator surface

### B8. UI re-mount

Stage 5 deleted the legacy embedded console. **Decide:** re-mount the
existing assets (if fit) or build fresh against the typed OpenAPI spec.
Size: ~1 day (re-mount) to ~2–4 weeks (rewrite). Where:
`app/httpapi/control/ui.go`, possibly a separate UI repo if rewriting.

### B9. Keypool admin observability

`GET /host-keys/by-id/{id}/health` returning current breaker state
(circuit, failure counter, cooldown deadline, last failure kind, last
success time). ~half day. Where: `app/httpapi/control/host_keys.go`
extension; `app/keypool` exposes a snapshot accessor.

### B10. Slug edit + referencing-rewriter

Operators can't rename catalog rows today. Adding the UPDATE path requires
walking every spec field that carries a slug ref (cross-refs store ids,
but human-facing slugs still need rewriting). ~3 days. Where:
`app/manifest/translate.go`, `app/httpapi/control/crud.go`.

### B11. MFA / password reset

TOTP + email-link password reset on the console. DIY or adopt
SuperTokens / Kratos (decide alongside B5 SSO — likely the same vendor
call). Required before any self-serve exposure. Where: `app/user`,
`app/httpapi/control/auth.go`.

---

## Deployment, hardening & readiness

### B12. Observability wired (consumes OSS A4)

Take the OTel tracing + Prometheus observers that land in
`roadmap-oss.md` (A4) and the usage→ClickHouse sink (shipped) and wire
them into a *supported* on-prem deployment story: dashboards, alert rules,
the metrics/traces endpoints documented and secured. The code is OSS; the
*productionised, documented, supported* wiring is the enterprise
deliverable.

### B13. License / entitlement mechanism

Gate enterprise features (SSO, audit, advanced authz) behind a
license/entitlement check. Mechanism decision: signed offline license file
vs phone-home. Air-gap (B16) forces offline-capable. ~1 week.

### B14. Helm chart + TLS + secrets/config + HA

The *hardened* Helm chart (the OSS track ships starter manifests). TLS
termination, secrets/config management (reuses `pkg/secret` external
backends), HA docs (multi-pod, `RELAY_CLUSTER_MODE` already default-on,
NOTIFY-sync within ~1s). ~1–2 weeks.

### B15. Backup/restore + upgrade/migration runbooks

Postgres backup/restore, ClickHouse retention, the migration story
(`migrations/postgres/` versioned up+down), zero/low-downtime upgrade
procedure. Runbook docs + tested procedures. ~1 week.

### B16. Air-gapped install

Finish the offline catalog/deploy story. The `go:embed`'d default catalog
exists for first-boot; this completes it: offline image bundles, offline
license (B13), no-egress telemetry default, documented air-gap install.
~1 week.

### B17. Perf SLO gate (consumes OSS A7)

Wire the existing `bench/pipeline/` harness into CI as a regression gate
with documented baselines (the OSS A7 item). The enterprise framing: this
is the *contractual* perf SLO (live: p50 < 2ms, p99 10ms internal / 15ms
public). Same code, formalised as a gate. ~half day.

### B18. Security hardening pass

Container hardening (distroless/non-root, read-only FS), network policy,
a written threat model. The header-leakage class is already closed (A4
security-leakage test — relay key + `X-WR-*` no longer leak to upstreams).
~1 week.

### B19. Security-review readiness

Vuln scanning in CI (govulncheck + image scan), a SOC2-lite checklist,
pen-test prep. Pairs with B6 (audit) + B18 (hardening). ~1 week + ongoing.

### B20. Operator / install / support docs + onboarding kit

The operator runbook, install guide, support docs, and a design-partner
onboarding kit. The artifact that makes an on-prem sale supportable.
~ongoing.

---

## Dependencies

- **Consumes from OSS** (`roadmap-oss.md`): A4 observability observers
  (→ B12), A7 bench harness (→ B17).
- **Feeds SaaS** (`roadmap-saas.md`): B1 users, B3 authz, B7 light org
  scoping are the seams the SaaS multi-tenancy core widens. Build the
  seams here correctly and the SaaS track is an extension, not a rewrite.
