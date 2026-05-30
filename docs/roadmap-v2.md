# Roadmap — beyond v1

Everything here is **off the v1 critical path**. The three v1 tracks
(`roadmap-oss.md`, `roadmap-enterprise.md`, `roadmap-saas.md`) come first.
This doc holds two kinds of not-now:

- **V2 — beyond the wedge.** Bigger than a feature; its own product line.
  Finish the v1 infra story before it competes for priority.
- **Icebox — deferred indefinitely.** Considered, found not to clear the
  bar, parked. Touch only if a concrete external signal flips the call.

---

## V2 — beyond the wedge

### Tool Gateway (capability-parity layer)

A separate service that canonicalizes *tool* I/O the way Relay
canonicalizes *LLM wire* I/O — one tool contract, many backends:

- **Search** — Brave / Serper / SearXNG.
- **Code execution** — a sandbox.
- **Fetch** — a guarded `web_fetch` (SSRF-defended).

Hosted or self-hosted, **in its own pods, never on the hot path**.

**Goal:** give Ollama and other non-frontier models the server-side tools
that only frontier closed models ship today. Out-positions OpenRouter on
an axis they don't compete on.

**Why it's V2, not v1:** it's a second product surface with its own
canonical contract, threat model (SSRF on the fetcher especially), and
backend integrations. It doesn't make the core router better — it adds a
new capability *next to* it. Ship the infra wedge first.

Full proposal — subsystem boundary, canonical surface, SSRF threat model,
open questions — in `docs/v2-tool-gateway.md`.

---

## Icebox — deferred indefinitely

These were considered, found not to clear the bar, and parked. Each has a
named **unblock signal** — the concrete external thing that would flip the
call. Don't casually promote them.

### Cross-shape translation (inbound ≠ binding shape)

`/v1/chat/completions` for a model whose binding declares
`adapter: anthropic`, with relay translating shapes via the canonical hub.
Currently returns 400. Per CLAUDE.md: "same-format passthrough is the 95%
case." The canonical chain machinery exists (it's how upstream-only
adapters like Gemini are reached); this would just open it on the inbound
edge.

**Unblock signal:** real customer traffic where wrong-shape requests are
>5% of a meaningful tenant's load.

### Quota-aware key selection

A per-key budget tracker, fed by usage observations, that biases
`Selector.Pick` toward keys with headroom. Existing algos (prioritized /
round-robin / LRU) already cover "drain key #1 first then fall over."
Quota-awareness adds an observations-feedback loop without clear demand.

**Unblock signal:** a real operator request with a specific scenario the
prioritized algo can't model.

### Pluggable selection strategies

A plugin-style registry for custom selection algos (cost-tier-aware,
sticky-per-user, latency-aware). Today `KeySelection` is a closed enum
that covers known use cases. Plugin infrastructure before a concrete
second-party algo is over-engineering.

**Unblock signal:** a customer who can articulate "I want this specific
algo and we will fork to add it."

### Marketplace / token reselling

Per CLAUDE.md's wedge definition, explicitly **not** what Relay is —
deferred indefinitely. Relay is BYO-key infrastructure, not a reseller of
provider tokens.

**Unblock signal:** a deliberate strategy change, not a feature request.
