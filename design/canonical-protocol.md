# Canonical Protocol — design doc

**Status**: **Phase 2 shipped and stable.** `sdk/v1/` is the canonical protocol package; the OpenAI and Anthropic vendor adapters target it via the `v1.Translator` interface; the generic `app/adapter/` framework provides `Spec` + `Registry` + dispatch glue. Verified end-to-end via `make smoke-mock` (recorded fixture replay) and live tool-use round-trips, with cross-shape routes (OpenAI Responses ↔ Chat ↔ Anthropic) in production use.

This doc continues as the living source for protocol decisions; the code now reflects the doc (was: doc preceded code). Living document — decisions land here when made.

This doc covers the relay's **canonical inbound protocol** — the API the relay exposes on the bare `/v1/*` namespace as its own wire shape, independent of OpenAI / Anthropic / Gemini specifics.

It is not just a request/response shape. It defines:

- Tool taxonomy (function / server / mcp)
- Execution model (who runs what, when)
- Multi-step semantics (one HTTP request = one or N model calls under the hood)
- Streaming protocol (events synthesized across an agentic loop)
- Credential and ownership boundaries
- Billing and rate-limit granularity
- Security model for server-side tool execution

Treat the canonical as a **protocol**, not an endpoint.

---

## Why we're doing this

OpenAI's Responses API consolidated their server-side capabilities (web_search, code_interpreter, file_search, computer_use, image_generation, mcp) into a single endpoint shape. Anthropic added equivalents (web_search, code_execution) under their existing Messages API. The industry direction is clear: LLM APIs are accreting agentic capabilities — server tools, MCP integration, multi-step transcripts — and customers expect them as first-class features.

If the relay only translates wire shapes, customers reach for a separate agentic layer (orchestration frameworks / vendor-direct). If the relay owns the protocol, that layer can live here too.

The constraint: we don't want to be locked into OpenAI's evolutionary path. They've shipped seven business-tier and infrastructure-specific knobs since Responses launched (`phase`, `safety_identifier`, `service_tier`, `conversation`, `context_management`, `encrypted_content`, `prompt_cache_key`). Their API absorbs their business logic. Ours should not.

So: we adopt Responses' good ideas (typed item arrays, symmetric input/output, server-side tool concept), define our own canonical with extension points, and translate to/from every vendor at the adapter boundary.

---

## Scope

### In v1

- Single canonical wire shape on `/v1/generate` (or final naming TBD)
- Item-typed union (input/output) — closed set of types we model
- Function tools (caller implements; relay routes the call)
- Streaming protocol with synthesized events for relay-side activity
- Stateless: no `previous_response_id`, no `store`, no server-side conversation memory

### In v2 (planned, not implemented)

- Server tools (relay-implemented `web_search`, `code_execution`, etc.)
- MCP integration (relay as MCP proxy/gateway)
- Multi-step agentic loops (one HTTP request → N model calls internally)
- Per-tool execution credentials and operator config

### Deferred indefinitely

- Stateful conversations / session memory
- Background async mode (`background: true` style polling)
- Logprobs (vendor-specific surface; no clean cross-vendor story)
- Cross-vendor `previous_response_id` equivalent

The deferred items aren't impossible — they're just not paying for their complexity yet. They can be added if a real customer ask appears.

---

## Wire shape principles

These are the rules the canonical's design follows:

1. **Symmetric input/output**: input items and output items share most types. What you receive in `output[]` you can splice into the next request's `input[]`. No `tool_call_id` correlation gymnastics like Chat Completions.

2. **Typed discriminated unions**: every polymorphic field (`Item`, `Part`, `Tool`, `Event`) is a closed sum type. Implementations live in the canonical package; nobody outside can implement, so type switches are exhaustive.

3. **Boundary narrowing**: the canonical types only model what we accept. Anything outside is rejected at parse with explicit error messages. Forward-compat fields (the OpenAI-isms we want to keep wire-compatible with) are accepted but stored as raw bytes, never plumbed through the pipeline.

4. **No request-echo in response**: the response carries only what the response produced. If you want to know what you sent, you have it locally.

5. **Snake-case on wire, CamelCase in Go**: JSON tag is the single source of truth.

6. **String-or-array content normalizes to array internally**: wire allows `"content": "hi"` for terse single-text messages. Internal types always have `[]Part{TextPart{Text:"hi"}}`. Translators never see the string form.

7. **Stateless protocol contract**: relay does not promise to remember anything between HTTP requests. Each request is self-contained. If a future v2 adds server-side state, it's a separate opt-in feature, not the default.

---

## Codebase rules (non-negotiable)

These rules govern how the canonical protocol relates to the rest of the repo. Phase 2 work and everything after must obey them.

1. **Canonical knows nothing.** `sdk/v1/` (the canonical protocol package) declares its own types, its `Translator` interface, its `Name` + `Registry`, and nothing else. It does not import any `sdk/adapters/<vendor>/`, any `app/` package, or any vendor-named symbol. It is the lowest layer.

2. **Vendors import canonical.** Each `sdk/adapters/<vendor>/` package imports `sdk/v1/` and implements its `Translator` interface. Vendor adapters never import each other.

3. **One folder per vendor, not per wire shape.** `sdk/adapters/openai/` owns *all* OpenAI wire shapes (Chat Completions, Responses, Embeddings, …) as files within the package. Adding a second wire shape for a vendor is a new file in the existing folder, never a new sibling package. Wire-shape names never appear in folder paths.

4. **No vendor names in `app/` code.** Grep test: `grep -rE "openai|anthropic|gemini|cohere" app/ --include="*.go"` returns only catalog data references (string lookups against persisted names), error messages, and the composition root (`cmd/relay/main.go`). Dispatch, routing, pipeline, registry, http-mw — none of them branch on or import a vendor.

5. **Composition root is the only place vendor names appear in code.** `cmd/relay/main.go` imports the vendor adapter packages and registers them with the `app/adapter/` framework. Every other binary, test, or service consumes adapters through the `Registry`.

6. **Adapters are stateless pure transforms.** A `Translator` is six methods (parse/serialize × request/response, plus the two stream factories). No per-request state is held on the `Translator` value itself — the stream factories return per-stream closures, but the `Translator` instance is reused across requests. Per-request data the wire renderer needs (e.g. OpenAI Responses' request-echo fields) is passed explicitly through the call signature.

7. **`extensions` envelope for cross-cutting concerns.** Anything that doesn't map cleanly across all vendors (safety settings, RAG document context) lives in the `extensions: map[string]json.RawMessage` envelope on `Request` and `Response`. Vendor adapters that understand a key emit the corresponding wire field; adapters that don't, ignore it. No new top-level canonical field for *vendor-specific* features.

   **Exception — prompt caching is first-class.** Caching is *not* vendor-specific: both OpenAI and Anthropic cache, and the intent ("this prefix is stable") maps cleanly across them. It therefore earns a first-class, intent-named, vendor-neutral home rather than the envelope: `Request.CacheConfig` (`cache_config`, an object with `instructions`/`tools` knobs, mirroring `model_config`) and a per-item `cache_config` (`ItemCacheConfig{anchor}`) for the rolling "stable history up to here" anchor. No per-vendor cache vocabulary (`cache_control`, `prompt_cache_key`, ephemeral/persistent) ever reaches canonical — the caller sets identical values regardless of route. Supporting adapters (Anthropic) translate the intent into wire breakpoints; adapters whose vendor caches automatically (OpenAI) ignore it as a silent no-op. Cache effectiveness is read back via the already-normalized `Response.Usage["cache_read"]`/`["cache_creation"]` meters. The general rule still holds: a *vendor-specific* knob (no clean cross-vendor intent) goes in `extensions`, not a new field.

8. **`provider_data` for same-vendor opaque blobs.** Signed/encrypted vendor payloads (Anthropic thinking signatures, OpenAI `encrypted_content`, etc.) are carried on the relevant item (`reasoning`, `tool_call`, `message`) as a `provider_data json.RawMessage` field. Round-tripped verbatim when going back to the same vendor; dropped when going cross-vendor. Customers never construct it.

9. **Refusal is a stop_reason, not an item type.** Doc principle 4 (item taxonomy section): the model's refusal text appears as a normal `message` item's text content with `finish_reason: "refusal"` on the response. There is no `refusal_part` type.

10. **SDK module purity preserved.** The `sdk/` module (`github.com/wyolet/relay/sdk`) imports nothing from `app/` or `internal/`. It is a standalone, vendorable library — `v1` (canonical), `adapters/*`, `usage`, `catalog`, `client` — that someone external can `go get` and use without pulling the Relay server (no pgx/redis/clickhouse, courtesy of Go module pruning). The server module depends on `sdk` (via `replace ./sdk` + a root `go.work` for dev); the direction is **server → sdk, never reverse**. The embedded catalog (`sdk/catalog/catalog.json`, generated by `cmd/catalog-embed`) crosses the boundary as a generated data file, not an import edge. (`pkg/` likewise imports nothing from `app/`/`internal/`.)

11. **No silent drops.** An adapter that receives canonical input (or upstream output) it cannot express on the other side must never accept-and-discard it silently. It must do one of: (a) **emit** it on the wire where a mapping exists; (b) **carry** it in `provider_data` (same-vendor opaque) or `extensions` (cross-cutting) per rules 7–8; (c) **annotate** an intentional, irreducible drop with a `// canonical: <field> dropped — <why>` comment at the drop site (greppable, so the remaining intentional drops stay auditable); or (d) for **safety-relevant** signals — an unmapped finish/stop reason, a content-filter block, a refusal — surface it (distinct `finish_reason`/`status`/`incomplete_details`) rather than let it masquerade as a clean success. Silent accept-and-discard is the exact bug class the fidelity audits (`design/adapters/`) found across every adapter. Note: fully *automated* runtime warning emission (a structured `adapter_drop` event) is deferred — translators are pure functions with no logger, so it needs a drop-sink threaded through the call signature; until then this rule is enforced by the comment convention + code review + the per-adapter audits.

---

## `relay_usage` — a relay-produced response field (the one schema escape hatch)

`Response.RelayUsage` (`relay_usage`, optional) is the single field on the
canonical response that **no vendor adapter ever populates**. It's relay's own
per-request observability — token counts, finish reason, retry `attempts`,
`streamed`, and the anchored `timing` breakdown (`upstream.{start,
response_start, response_end}` + `end`, all µs from the request-start anchor;
derive TTFT/relay-overhead/upstream-duration by subtracting, never chained).
It's a public *subset* of the internal usage record — no relay-key hash, no
policy/host/model UUIDs.

It's a deliberate, documented tension with rules 1 and 7, resolved like the
`CacheConfig` carve-out:

- **Canonical-only — vendor shapes never carry it.** A caller speaking the
  OpenAI/Anthropic shape gets a clean vendor response with nothing added; only
  a **canonical** caller (`adapters.Canonical`, the `/v1/*` shape) gets
  `relay_usage`. relay's telemetry rides relay's protocol — it is *not*
  injected into vendor bodies. This keeps every vendor adapter byte-faithful
  and sidesteps the whole "did we break a vendor SDK's parser" question.
- **Why it's in the schema (not pure injection):** if the canonical *type*
  didn't declare it, our own `sdk/client` would be blind to a field
  that's on the wire and the schema would lie — a defect for a vendorable
  library + client. So the type declares it (rule 1's letter holds —
  `sdk/v1` declares its *own* `RelayUsage` type, imports nothing from
  `app/`). Relay-native and *typed*, like caching — not an opaque
  `extensions` `RawMessage`.
- **How it lands:**
  - **Buffered (`/v1/generate` non-stream):** relay sets the typed
    `Response.RelayUsage` field before the canonical (identity) serializer
    runs — a real serialized field, read natively by the client. No byte
    injection.
  - **Streaming (`/v1/generate` stream):** it rides the terminal
    `generation.completed` event as `GenerationCompletedEvent.RelayUsage` —
    spliced into that last frame at end-of-stream (one-frame lookahead),
    *never* a standalone trailing frame. The client reads it off the event it
    already parses.
- **Collection timing:** echo fills the usage producer pre-send (buffered) or
  via a `lifecycle.StreamObserver` that watches the upstream frames and
  produces at end-of-stream; either way the `lifecycle` fill is idempotent so
  the post-flight sink reuses the same collection (collect once). The echoed
  `timing.end` is "response-ready", not "fully transmitted" — client-egress
  tail excluded (it isn't relay overhead). Accepted.
- **Coverage:** canonical buffered + canonical streaming. A `gzip`'d body is a
  follow-up (the splice no-ops on non-JSON-object frames).

---

## Item taxonomy — v1

Closed union, 4 types. Adding a new item type requires a protocol bump.

| Type | Direction | Fields | Notes |
|---|---|---|---|
| `message` | input + output | role, content `[]Part` | role ∈ {user, assistant, system, developer} |
| `tool_call` | input (echo) + output (emitted) | id, name, arguments (JSON string) | function tool invocation |
| `tool_result` | input (caller provides) | tool_call_id, content `[]Part` or string | result for a prior tool_call |
| `reasoning` | input (echo) + output (emitted) | id, summary `[]Part`, content (string, optional) | model's thinking; opaque to relay |

### Refusal handling

A model refusal is a `finish_reason: refusal` on the response, not a separate item type. The text of the refusal lives in a normal `message` item's text content. This is stricter than OpenAI Responses (which has a typed `refusal` part) but matches Anthropic's approach. Rationale: refusal-as-item adds a third axis of "is this content/refusal/something-else" that translators have to negotiate; refusal-as-stop-reason is what every existing API surface already understands.

### What we don't model in v1

- `file_search_call` — no universal vector store
- `image_generation_call` — no universal image generator
- `web_search_call` (server-emitted) — until v2 server tools land
- `code_interpreter_call` (server-emitted) — same
- `computer_call` / `computer_call_output` — beta territory, narrow audience
- `mcp_call` / `mcp_list_tools` — until v2 MCP integration lands
- `compaction` — we don't do server-side compaction
- audio output items — no audio modality in scope

When v2 adds server tools and MCP, the relevant item types become first-class. The wire format should be additive (new item types in the union); existing v1 clients don't need to change.

---

## Content parts

Inside a message's `content[]`:

| Type | Used in | Fields |
|---|---|---|
| `text` | input + output | text |
| `image` | input | data (base64 or URL), media_type, detail (optional) |
| `file` | input | url, id, or data; media_type, filename |

For output text, we additionally allow `citations []Citation` per part. Citation types modeled in v1: `url_citation` (url + start + end), `text_citation` (start + end of cited text in upstream document). Future citation types are admitted as `{type, raw}` for forward-compat.

---

## Tool taxonomy

Closed union. Each tool kind has different execution semantics.

```
type Tool = FunctionTool | ServerTool | MCPTool
```

| Kind | Who executes | v1 status |
|---|---|---|
| `function` | Caller (customer's code receives `tool_call`, returns `tool_result` in next request) | ✅ v1 |
| `server` | Relay-internal — relay runs the tool, injects `tool_result` automatically, calls model again | ❌ v2 (wire-modeled, runtime-rejected) |
| `mcp` | Relay-as-proxy to a customer-declared or operator-configured MCP server | ❌ v2 (wire-modeled, runtime-rejected) |

The wire format admits all three kinds from v1; v1 runtime rejects `server` and `mcp` with `tool_kind_not_implemented` so future support is additive, not breaking.

### v2 server tools (planned)

When implemented, server tools relay can offer:

- `web_search` — relay calls an operator-configured search API (Brave / Tavily / SERP)
- `code_execution` — relay runs Python in a sandbox (Pyodide local, or outsourced to E2B / Modal — operator picks)
- `bash` — sandbox-restricted shell
- `file_search` — vector store managed per-customer (operator-configured backend)

Operators enable each server tool individually in the catalog (same pattern as enabling a Host). Customers reference enabled server tools in their requests via the canonical name (`{kind: "server", name: "web_search"}`).

### v2 MCP integration (planned)

Two paths to be decided:

- **Per-request**: customer declares `{kind: "mcp", server_url, headers}` in the tools array. Relay establishes the MCP connection, advertises tools to the model, proxies calls.
- **Operator-configured**: operator pre-registers MCP servers in the catalog with friendly names; customer references by name.

Likely answer: support both. Operator-configured for trusted/common MCPs (auth lives operator-side); per-request for ad-hoc.

---

## Execution model

### v1: caller-side function tools only

For function tools, the relay's job is simple: parse the request, forward to the model, return the model's `tool_call` items in the response. The caller (customer's code) executes the function and submits a follow-up request with the `tool_result` item appended to `input[]`.

One HTTP request → one model call. No internal loop. No agentic execution. This is the same execution model as OpenAI Chat Completions and Anthropic Messages today.

### v2: relay-side server tools

When server tools land, one HTTP request can result in **N model calls** under the hood:

```
1. POST /v1/generate (customer's request with server tools enabled)
2. relay → model → response with tool_call items for server tools
3. relay executes each server tool (web search, code exec, etc.)
4. relay synthesizes tool_result items
5. relay → model → response (continuing the loop)
6. ... repeat until model emits a final message
7. response: full transcript in output[], usage summed across all model calls
```

This is the **agentic runtime** aspect. Customers using server tools opt in; customers using only function tools see the same one-request-one-model-call semantics as v1.

### Transcript visibility

The response `output[]` carries the **full ordered transcript** of all activity during the request. Customers see every model call's output and every relay-executed tool's result, in order. No "summary vs verbose" mode — there's one truth and that's what we return.

Rationale: agentic requests are by nature multi-step; debugging requires the full trace; customers will demand it; storing a one-truth response simplifies billing reconciliation.

### Loop termination

Hard limits to be locked before v2 ships:

- Max iterations: ??? (proposed: 10)
- Max wall-clock: ??? (proposed: 90 seconds; configurable per policy)
- Tool execution timeout: ??? (proposed: 30s per tool)
- Infinite-loop detection: ??? (heuristic — same tool_call N times in a row)

Each can be a per-policy setting; defaults shipped sane.

---

## Streaming protocol

### v1: single model call

Stream events are emitted as the model produces output. Six event types collapse OpenAI Responses' ~50:

| Event | Purpose |
|---|---|
| `generation.created` | Start; carries response id + model |
| `item.started` | A new item begins (message, tool_call, reasoning) |
| `item.delta` | Chunk into the current item; discriminated by parent item kind (text, arguments JSON, reasoning text) |
| `item.completed` | Item finished |
| `generation.completed` | All done; carries final usage |
| `error` | Stream-level failure |

### v2: multi-step loops

When the relay runs an internal loop, events flow as they happen across the entire transcript. The client sees one continuous stream:

```
generation.created
item.started (message: assistant)
item.delta (text)
item.delta (text)
item.completed
item.started (tool_call: web_search)  // model emitted server tool call
item.completed
item.started (tool_result)             // relay synthesized after executing
item.delta (text content)
item.completed
item.started (message: assistant)      // second model call
item.delta (text)
...
generation.completed (with full usage across all model calls)
```

Order matches transcript order. There is no distinction between "model emitted" and "relay synthesized" events — clients just see items.

### Backpressure

When the relay is mid-tool-execution (server tool or MCP call), the model isn't producing tokens. **Open question**: do we emit keepalive events to the client, or just go quiet? Proposed: emit `item.delta` with empty payload every 10s during tool execution, so HTTP/2 + load balancers don't close the connection. Tied to tool execution timeout discussion.

---

## Tool resolution

How does a request reference a tool?

### Function tools — inline

Tool definitions are **task-level**, not per-model: they live in a single
top-level `tools` object (`Request.Tools`, a `ToolsConfig{definitions, choice,
parallel}`), shared by every model in a multiplex request. They do **not** sit
under `model_config` — that map is reserved for per-model knobs (`sampling`,
`reasoning`, `output`) and relay-level options that may not reach upstream. One
spec, defined once; each upstream adapter renders it onto its own wire `tools`
field and decides how.

```json
{
  "model": "gpt-5.5",
  "input": [ ... ],
  "tools": {
    "definitions": [
      { "type": "function", "name": "get_weather", "description": "...", "parameters": {} }
    ],
    "choice": { "mode": "auto" },
    "parallel": true
  },
  "model_config": { "gpt-5.5": { "reasoning": { "effort": "high" } } }
}
```

Each definition is one of:
```json
{
  "type": "function",
  "name": "get_weather",
  "description": "...",
  "parameters": {...}
}
```

### Server tools — by name from catalog (v2)

Catalog has a `ServerTool` entity (TBD; akin to Host/Model/Policy). Operators enable specific server tools, configure their backend (Brave key, sandbox provider, etc.):

```json
{
  "type": "server",
  "name": "web_search"
}
```

The relay looks up "web_search" in the operator's enabled-server-tools list, applies the operator's config, executes.

### MCP — by URL or catalog name (v2)

Per-request, ad-hoc:
```json
{
  "type": "mcp",
  "server_url": "https://mcp.example.com",
  "headers": {"Authorization": "..."}
}
```

Or via catalog reference:
```json
{
  "type": "mcp",
  "name": "operator-configured-mcp-server"
}
```

The MCP server's tool definitions are advertised to the model on the relay's behalf. The model emits `tool_call` items for MCP tools; relay forwards to the MCP server; MCP server returns results; relay continues.

---

## Credentials and ownership

### v1: customer auth → relay; relay → upstream LLM

Standard pattern. Customer sends bearer relay key. Relay looks up key → policy → host. Relay uses operator-configured host keys (env-ref or AES-encrypted) for upstream calls.

### v2 additions

- **Server tool credentials**: operator config. Brave Search API key, code sandbox provider key, etc. lives in the catalog (encrypted). Customer doesn't supply.
- **MCP credentials**: either operator-configured (in catalog, encrypted) or per-request (in `headers` field of the `mcp` tool config). For per-request, relay forwards verbatim; no storage.
- **Billing back to customer**: each server tool execution and each MCP tool call generates a usage event. Billing model TBD (see next section).

---

## Billing and rate-limiting

### Granularity choices

Three possible billing units:

1. **Per HTTP request** — flat charge per `/v1/generate` call. Simple but punishes light requests.
2. **Per model call** — N model calls in an agentic loop = N model-call billing entries. Honest about cost.
3. **Per tool execution** — N tool executions = N tool entries (operator-set price per tool kind).

**Proposed v2 model**: separate line items.
- Model call: token-based pricing (input + output + cached + reasoning), per the catalog Pricing entity
- Server tool execution: operator-configurable per-execution fee (web_search = $0.005, code_execution = $0.01, etc.)
- MCP tool execution: no relay-side fee by default; operator can configure if they want

Usage events emit per model call AND per tool execution. The final response's `usage` field aggregates across the entire request.

### Rate limiting

Today's rate limits are per-relay-key per-policy per-meter (requests, tokens). When v2 agentic requests run multiple model calls, we need to decide:

- Does rate limit reservation happen once at request start, or per model call?
- Per-tool rate limits (e.g., "max 10 web searches per minute") — separate dimension?

**Proposed**: rate-limit reservation per HTTP request (one charge for `requests` meter), plus per model call for tokens (so a 10-step agentic request counts as 1 request + sum of all tokens). Per-tool rate limits added as a separate meter set in v2.

---

## Failure semantics

### v1

- Upstream LLM 4xx → propagate to caller as-is with our error wrapper
- Upstream LLM 5xx → retry next key in pool; if exhausted, 5xx to caller
- Parse error / unsupported field → 400 from relay
- Auth / rate limit / policy violations → 401/403/429 from relay

### v2 additions (multi-step)

- **Tool execution failure** mid-loop: surface to model as a `tool_result` with `is_error: true` content. Let the model decide to retry, swap tools, or give up. This is the LangGraph-style "let the model recover" pattern.
- **Max iterations exceeded**: response status = `incomplete`, with `incomplete_details.reason = "max_iterations"`. Customer sees the partial transcript.
- **Wall-clock timeout**: response status = `incomplete`, with `incomplete_details.reason = "timeout"`.
- **MCP server unreachable**: same as tool execution failure — relay synthesizes a `tool_result` with error content for the model to see. If the MCP failure happens at tool-listing time (before the first model call), 502 to caller.

---

## Security

### v1

- Standard relay surface; nothing new.

### v2 (server tools and MCP)

Significant new surface:

- **Sandbox model for code_execution**: must be isolated (no host filesystem access, no network egress except whitelist, kernel-level cgroup limits). Options: Pyodide in-process (simple, slow), gVisor / Firecracker container (medium complexity, real isolation), outsource to E2B / Modal (fast but new vendor dep).
- **Network egress from server tools**: web_search needs internet; code_execution should default to no-network-unless-asked.
- **MCP server validation**: arbitrary URL + headers means the customer can point relay at any MCP server. Risk: relay becomes an SSRF tool. Mitigation: per-policy allowlist of MCP server URLs (operator opts in to allowing arbitrary URLs).
- **Secret handling**: MCP `headers` field may contain customer secrets. Don't log them. Don't store. Forward only.

---

## Open questions

Listed as the doc evolves. Each gets a decision (with reasoning) when we lock it. Until then, sub-headings stand.

1. **Endpoint name** — `/v1/generate` vs alternatives. Pending.
2. **Apiversion in body** — none, just `"object": "generation"` in response. Provisional.
3. **Loop termination defaults** — max iterations (10?), wall-clock (90s?), per-tool timeout (30s?). Pending.
4. **Backpressure during tool execution** — keepalive events at fixed interval vs go-quiet. Pending; need user-feedback signal.
5. **MCP credentials model** — per-request only, catalog only, or both. Pending; need real customer use case.
6. **Sandbox provider for code_execution** — in-process Pyodide / hosted (E2B / Modal) / containerized. Pending; depends on operational appetite.
7. **Server tool catalog entity** — new kind in the catalog YAML schema, or extension of Host. Pending.
8. **Per-tool billing structure** — operator-set flat fees vs token-based vs subscription. Pending.
9. **Rate limit granularity in agentic loops** — per request / per model call / per tool. Provisionally per-request + per-token.
10. **OTel span structure** — one span per HTTP request (with child spans for each model call and tool exec) is the obvious model; confirm.
11. **`encrypted_content` from OpenAI Responses upstream** — what we do with it when we receive it (in Phase 2 work). Provisionally: forward verbatim when round-tripping to same upstream; drop otherwise.
12. **Cross-shape translation losses to document publicly** — when canonical → vendor X drops field Y, where does that documentation live (per-field in this doc, or in the adapter README, or both).

---

## Phase history

- **Phase 1** (PR #175): `/openai/v1/responses` byte-pass inbound. No canonical work; just routes Responses bytes to OpenAI's `/v1/responses` upstream.
- **Phase 1.5** (PRs #176–#183): hand-written Responses Go types in `sdk/adapters/openai/responses/` + pairwise `cctranslator` / `anthropictranslator` packages that did Responses↔CC and Responses↔Anthropic directly. The design of these types was the testbed for canonical.
- **Phase 2 — shipped 2026-05-22** (PRs #185–#189):
  - PR #185 — canonical types extracted into `sdk/v1/`; narrowed-Responses (drop the 9 stateful OpenAI-isms; refusal becomes a stop_reason; add `extensions` envelope and `provider_data` opaque field); `v1.Translator` interface defined.
  - PR #186 — `sdk/adapters/openai/` rewritten against canonical; both CC and Responses wire shapes target canonical; `cctranslator/` deleted; OpenAI Responses inbound cuts over to the canonical chain.
  - PR #187 — `sdk/adapters/anthropic/` rewritten against canonical; `anthropictranslator/` deleted; all four Anthropic-touching dispatch paths now go through canonical.
  - PR #189 — generic `app/adapter/` framework + collapse of `app/adapters/<vendor>/` packages + deletion of `Deps.CrossShapeHandlers`. `dispatch.go` is now fully shape-agnostic.

Verification:
- Unit translator coverage: 67–73% on OpenAI / Anthropic canonical translators (~100 round-trip tests including composed E2E).
- `make smoke-mock` replays recorded OpenAI fixtures (parallel tool-calls + 146-chunk SSE) through `relay → openai-mock.wyolet.dev`; byte-identical end-to-end.
- Live Claude Code → relay → ollama-self/gpt-oss-120b tool-use round-trip verified (caught + fixed one real bug — `content_block_start` shape for tool_use blocks was missing `id`/`name`/`input` fields; PR #189 fix `5e1a26c`).

---

## How to update this doc

When making a decision:
1. Resolve an open question, move it from "Open questions" to its proper section as a decision.
2. Note the date and a one-line rationale.
3. Don't reformat the whole doc; preserve git diff readability.
4. Memory: a project-memory pointer to this doc lives in `MEMORY.md` under `project-canonical-protocol.md`. Update that memory note if the framing shifts (the memory is for "what is this work and why"; this doc is for "what specifically").

When the doc is no longer the source of truth (because Phase 2 ships and the code is the truth), this file moves to `design/archive/canonical-protocol-v1-draft.md` with a `## Status: archived` note up top.
