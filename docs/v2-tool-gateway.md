# V2 — Tool Gateway (capability parity layer)

**Status:** proposal / napkin → v2 track. Not on the current roadmap's
critical path (Now #1–4). Written so the shape is captured before it
competes for priority.

**One line:** a separate service that canonicalizes *tool* I/O the same
way Relay canonicalizes *LLM wire* I/O — one tool contract, many
backends (Brave / Serper / SearXNG for search, a sandbox for code
execution, a fetcher for `web_fetch`), hosted or self-hosted, in its own
pods. The product goal is **capability parity**: give Ollama and other
non-frontier models the server-side tools that only frontier closed
models ship today.

## Why this is a wedge, not a feature

OpenRouter and LiteLLM *route*; they don't *execute* tools. A customer
who wants web search on an open model wires it themselves, per model,
per provider. The gap people actually feel with Ollama is exactly this:
no native `web_search`, no `web_fetch`, no code interpreter.

A canonical tool surface — satisfied natively where the vendor has it,
gateway-executed where it doesn't — smooths that ragged edge. The
customer codes against one capability contract and stops caring whether
the underlying model is frontier or local. That out-positions OpenRouter
on an axis they don't compete on, and it doesn't violate any v1 non-goal
(not a marketplace, not adaptive routing, not a custom wire protocol).

## What it is NOT (boundary, non-negotiable)

This mirrors the locked hot-path rules and exists to protect them.

1. **Not in the Relay request pipeline.** Giving a non-native model a
   tool means running an *agentic loop*: model emits a tool_call →
   gateway executes → feeds the result back → model continues → maybe
   loops. That is N upstream round-trips plus one unbounded-latency I/O
   hop (a fetch can hang 30s) per customer request. It cannot sit on a
   path whose identity is `<2ms p50`. The pipeline stays pure, fast,
   stateless.
2. **Not the same pod as Relay, not the same pod as the backends.** The
   gateway is its own subsystem with its own pods; search/exec backends
   are external dependencies it calls out to, never co-located.
3. **Not a stateful session.** Like the WS multiplexer, any orchestration
   is *request-scoped*, not session-scoped — each model turn re-sends the
   full conversation. No long-lived agent state.
4. **Not a middleware/plugin chain inside Relay.** The "No
   middleware/plugin chain à la LiteLLM" rule holds. Smart middle, yes —
   but a *new* middle, beside the router, not bolted into it.

## Architecture — a fourth subsystem

Today:

```
Realtime data plane | Batch workers | Control plane
```

Add:

```
                    + Tool Gateway (capability layer)
```

The gateway sits *in front of* the data plane and uses it as upstream.
Two modes, customer-selectable per request:

- **Tool-API mode (the core product).** Direct canonical tool calls:
  `POST /tools/web_search`, `POST /tools/web_fetch`,
  `POST /tools/code_exec`. Canonical request in, canonical result out,
  backend abstracted. This is useful standalone — any caller (including
  someone *not* using Relay) can normalize across Brave/Serper/SearXNG
  through one shape. This is where the canonicalization value lives.

- **Loop mode (opt-in interception).** "Run this prompt on `ollama/...`
  with `web_search` enabled." The gateway speaks the same
  OpenAI/Anthropic/canonical wire shape, runs the agentic loop, and
  calls `Pipeline.Run` (via Relay's normal `/v1/*` edge) as its upstream
  for each model turn — executing tool_calls against its own Tool-API
  mode between turns. Off by default; the customer opts in by enabling
  gateway-provided tools on a non-native host.

The symmetry to lean on: `sdk/v1/` is the canonical *LLM*
protocol with vendor `Translator`s behind it. The tool gateway gets a
canonical *tool* protocol with backend adapters behind it — same purity
discipline (a vendorable tool-translation library; backends never import
each other).

## Canonical tool surface (v2 scope)

Priority order. Start narrow.

1. **`web_search`** — *the whole demo.* One backend call, no
   arbitrary-URL surface, tiny risk envelope. Backends: Brave, Serper,
   SearXNG (self-host). Canonical result: ranked list of
   `{title, url, snippet, published_at?}`. This alone closes the felt
   gap with Ollama.
2. **`web_fetch`** — high value, **high risk** (see threat model). Fetch
   a URL, return cleaned text/markdown + metadata. Gate behind the SSRF
   controls below; do not ship without them.
3. **`code_exec`** — a sandbox project of its own (gVisor / Firecracker /
   a managed backend). Defer until 1–2 are solid; scope separately.

Canonicalization means: one request/result schema per tool, backend
differences (Brave vs Serper ranking, SearXNG engines) hidden behind the
adapter, opaque backend extras carried in an `extensions`-style envelope
exactly like the LLM canonical does.

## The hard parts (these decide whether it's worth building)

The orchestration loop is the *easy* part. What takes real care:

1. **`web_fetch` is an SSRF cannon.** Server-side fetch of user-supplied
   URLs from inside the cluster is the single biggest risk. Required
   before it ships:
   - URL allow/denylist; block RFC-1918, link-local, loopback, and
     cloud metadata endpoints (169.254.169.254 et al.) — resolve then
     re-check to defeat DNS rebinding.
   - Hard per-request timeout, response size cap, redirect-count cap
     (re-validate the target on every redirect).
   - Egress isolation: the fetcher runs in a network context with no
     route to internal services.
2. **Loop control.** Max iterations, per-request tool-call budget, total
   wall-clock cap. A cheap looping model must not run up an unbounded
   search bill.
3. **Streaming semantics.** Loop mode muddies the SSE contract. Decide
   early: stream only the terminal turn, or surface intermediate
   tool-call deltas as events.
4. **Billing / observability.** One customer request = N upstream LLM
   calls + M tool executions + backend API costs. Usage events must
   attribute all of it. Ties into the deferred non-token pricing meters
   (`web_search`, `code_interpreter` units already named in the
   roadmap backlog).
5. **Backend build-vs-buy.** Brave/Serper are paid APIs; SearXNG is
   self-host with quality/ops tradeoffs. Support all three behind the
   adapter; let the operator choose.

## Open questions (resolve in the design doc before code)

- **Separate repo or `cmd/relay-tools/` here?** Leaning separate
  service, possibly separate repo, given zero hot-path coupling and an
  independent release cadence. (Respect the no-over-engineering rule —
  don't split modules without demonstrated need; revisit at build time.)
- **Auth model** — does the gateway reuse relay keys / policies, or have
  its own? Loop mode needs a relay credential to call `/v1/*` anyway.
- **Where does the canonical tool schema live** so both Relay (to
  passthrough native tools, #1 from the original discussion) and the
  gateway agree on shape? Candidate: a `pkg/tools/v1/` mirroring
  `sdk/v1/`.
- **Does loop mode reuse the batch worker pool**, or is it its own
  concurrency domain?

## Relationship to v1 native-tool passthrough

Distinct from gateway execution: Relay v1 should already pass
*provider-native* `web_search`/`web_fetch` through with full fidelity
(canonical rule 11, no silent drops) — frontier models execute those
server-side and Relay just round-trips the config + results. That work
belongs in the adapters, not here. The gateway is strictly for models
that *lack* a native tool. The two compose: a customer enables
`web_search` once, Relay routes natively where it can and defers to the
gateway where it can't.
