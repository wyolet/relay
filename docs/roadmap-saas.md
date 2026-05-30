# Roadmap — Hosted SaaS (Phase C)

**Goal:** the hosted, multi-tenant product. Customers sign up, bring their
own provider keys, and pay per usage. This is the highest-bar track — we
hold customers' provider keys and run a shared fleet, so isolation,
secrets custody, billing accuracy, abuse control, and compliance all have
to be production-grade simultaneously.

**Builds on Enterprise** (`roadmap-enterprise.md`): the SaaS multi-tenancy
core *widens* the single-org identity/authz/scoping seams from B1/B3/B7
into a full Org→Workspace→Project hierarchy. Don't start the
tenant-isolation work until those seams exist.

**Builds on OSS** (`roadmap-oss.md`): batch (A1) + webhooks (A2) are the
async primitives the customer experience leans on; usage read API +
ClickHouse sink (shipped) are the metering substrate; the multi-row
Pricing shape (A10) underpins mode-tier billing.

**Ordering principle:** isolation and metering before money. Get tenant
boundaries and accurate usage data correct *first* — billing on top of
leaky isolation or inaccurate metering is a liability, not a feature.

---

## Tenancy foundation

### C1. Multi-tenancy core — Org → Workspace → Project

**What:** every catalog row gains an Org owner; the session payload grows
an `active_org_id` (the actor slot is already reserved); RelayKey scopes
to a Project. This is the full hierarchy the enterprise track's B7 light
scoping deliberately stopped short of.

**Why:** the structural prerequisite for every other SaaS feature —
nothing can be isolated, metered, or billed without a tenant boundary.

**Size:** ~2 weeks. Backfill migration + snapshot restructuring is the
risky part — the in-memory snapshot must stay tenant-partitioned without
breaking the hot-path "no Postgres on the request path" rule.

**Where:** `app/org`, `app/workspace`, `app/project` (extends B7), backfill
migration, snapshot restructuring.

### C2. Tenant isolation

**What:** enforce isolation across data, keys, and pooling — **no
cross-tenant key pooling** (already a hot-path rule), per-tenant data
partitioning, per-tenant quotas. Verify with a dedicated isolation test
suite (the security-leakage test pattern from OSS A4, extended to
tenant boundaries).

**Why:** the single highest-stakes correctness property of the whole
product. A cross-tenant leak is existential.

**Size:** ~2 weeks including the isolation test suite.

**Where:** snapshot partitioning, `app/keypool` (already tenant-scoped),
`pkg/ratelimit` per-tenant tags, routing.

### C3. Tenant-aware authz

Extend the `Authorizer` (Casbin from B3) with org/role context. Every
authz decision now carries `(actor, org, action, resource)`. The actor
already has the `ActiveOrgID`/`Roles` slots. ~3–4 days. Where:
`app/authz`, session payload.

### C4. Self-serve signup + onboarding

Self-serve signup + email verify + org creation + onboarding flow. Reuses
B1 users + B11 MFA/password-reset. The funnel from "landed on site" to
"first successful `/v1` call." ~1 week. Where: `app/httpapi/control` signup
flow, email sender, onboarding state.

---

## Secrets, metering & money

### C5. Secrets-at-scale

**What:** secure custody of customers' provider keys at scale — KMS-backed
encryption, key rotation, compliance posture. Extends the existing
`pkg/secret` `stored` backend (AES-GCM in PG under `RELAY_MASTER_KEY`) to
a per-tenant KMS-envelope model so a single master-key compromise doesn't
expose every tenant.

**Why:** we hold customer provider keys — this is the compliance crux
(feeds C14). The single-master-key model is fine on-prem, not at SaaS
scale.

**Size:** ~2 weeks.

**Where:** `pkg/secret` (new KMS-envelope kind), `app/secret.Wire`.

### C6. Usage metering pipeline

**What:** turn the per-request usage events (already captured + landing in
the ClickHouse sink) into a billing-grade aggregation pipeline — usage API
→ rated, deduplicated, tenant-attributed billing aggregates. Cost is
derived from tokens × pricing in the sink (events stay pricing-free by
design); this layer adds the rating + rollup.

**Why:** billing accuracy depends entirely on this. The raw event stream
exists; the *billable aggregate* doesn't.

**Size:** ~1–2 weeks.

**Where:** `pkg/usage` aggregation, new rollup tables, `/usage` →
billing-export seam.

### C7. Mode-tier pricing (batch / priority / flex)

**What:** multiple Pricing rows per Model, distinguished by a tier
(`metadata.labels.tier` or a new `Pricing.Spec.Tier`). Request-side picker
reads a header (`X-Relay-Tier: batch`) or infers from context (batch flow
→ batch tier automatically). Cost computation reads the matched tier's
rates.

**Open design questions:** field on Pricing vs label on metadata vs
separate join? How does a Policy declare allowed tiers? Default-tier
fallback rule? Does pricing-by-tier survive a LiteLLM re-import?

**Why:** batch (OSS A1) needs this to honour the provider-batch 50%
discount; priority/flex tiers are a SaaS monetisation lever. Shares the
multi-row-Pricing shape with non-token meters (OSS A10). Design-doc-first.

**Size:** ~1 week. Could ship inline with batch or right after.

### C8. Billing integration

Stripe — metered/usage-based plans, invoicing. Consumes C6 aggregates.
~1–2 weeks. Where: `app/billing/`, Stripe webhook handler (reuses A2
webhook delivery infra in reverse).

### C9. Quota + overage enforcement + hard spend stops

Per-tenant quotas (C2) become *enforced* limits with overage rules and
hard spend stops — a runaway tenant can't rack an unbounded bill. Rides
the existing `pkg/ratelimit` Lua-reserve path with a spend dimension.
~1 week.

### C10. Abuse / fraud controls

Signup abuse, key abuse, anomaly detection, spend-limit tripwires.
Pairs with C9 (spend stops) + C15 (edge protection). ~1–2 weeks, ongoing.

---

## Customer & fleet experience

### C11. Customer dashboard

Multi-tenant relay-ui: usage, keys, billing, logs. Extends the B8 operator
console with tenant scoping + the customer-facing views. The biggest UI
build. ~3–6 weeks. Where: UI repo (decided in B8).

### C12. Hosted fleet

Autoscaling, productionised cluster mode (`RELAY_CLUSTER_MODE` already
default-on with NOTIFY-sync), multi-region story, leader election + Redis
Cluster client (noted as future in `docs/cluster.md`). ~ongoing infra.

### C13. Per-tenant observability + SRE

Per-tenant observability dashboards (built on OSS A4 OTel/Prom +
enterprise B12 wiring) + internal SRE dashboards + alerting. ~1–2 weeks.

### C14. Compliance — SOC2 / GDPR / data residency

The high bar, because we hold customer provider keys. Builds on enterprise
B6 (audit), B19 (security-review readiness), C5 (secrets-at-scale).
Data-residency may force per-region fleets (C12). ~ongoing, months.

### C15. Edge protection

DoS + runaway-spend guards on the hosted ingress.
`config/ratelimits/system.yaml` already declares relay-internal
admission/DoS rules — this productionises them at the SaaS edge + adds
spend-aware guards. ~1 week.

### C16. Lifecycle + payments ops

Downgrade / cancel / refund / dunning / tax. The unglamorous billing
operations that a real paid product can't skip. ~1–2 weeks. Where:
`app/billing/`, Stripe lifecycle webhooks.

### C17. Status page + incident process + SLA

Public status page, incident process, an uptime SLA backed by C13
monitoring. ~1 week + ongoing process.

### C18. Per-org cluster isolation

When a single tenant's traffic dominates, give them a dedicated pod group.
Mostly a deployment story on top of C12 — minimal code. Promote only when
a real tenant forces it.

### C19. Marketing site + pricing + funnel

Marketing site + pricing page + self-serve sign-up funnel + self-serve
docs. The top of the C4 onboarding funnel. ~ongoing.

---

## Dependencies

- **Builds on Enterprise** (`roadmap-enterprise.md`): C1 widens B1/B7; C3
  extends B3; C4 reuses B11; C14 builds on B6/B19.
- **Builds on OSS** (`roadmap-oss.md`): C7 needs A1 (batch) + shares A10's
  Pricing shape; C6 builds on the shipped usage sink; C8/C16 reuse A2
  webhook delivery infra; C13 builds on A4 observability.
