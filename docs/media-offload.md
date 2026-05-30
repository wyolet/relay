# Media offload — provider-side file references for large media

Status: **design / roadmap** (not implemented). Owner: TBD.

## Problem

LLM requests can be large — providers accept up to ~32 MB (Anthropic's
documented request ceiling). The pain is **not the text**: ~1M tokens is
only ~4–5 MB of JSON, and successive turns in a chat session are
near-identical, so text compresses well at rest (ClickHouse block
compression collapses the shared prefixes — see `docs/payload-logging.md`).

The real bandwidth sink is **media on multi-turn resends**. A 3 MB image
in the conversation history is re-uploaded on *every* turn (client → relay
→ provider). Thirty turns = ~90 MB of the same bytes, and base64 media
neither compresses nor caches the way text does.

Providers already solve this: each exposes a **file API** — upload a blob
once, reference it by id on every subsequent request. Relay can ride that
to turn an O(N) re-upload into O(1).

## What this is NOT

We considered, and **rejected**, having relay host media at its own URLs
and rewriting requests so the provider fetches from a relay bucket. It
fails on four counts:

1. **Text can't be offloaded anyway** — no provider fetches your
   `messages` array from a URL; it must be inline. Only media is
   referenceable.
2. **Trust/passthrough break** — parking customer media on an
   internet-reachable relay bucket is a privacy + data-residency surface
   the caller never consented to. Silent rewriting of a security-sensitive
   passthrough is exactly what the no-silent-drops / header-leakage rules
   exist to prevent.
3. **Availability inversion** — the provider's servers would fetch from
   relay's bucket *mid-inference*; if the bucket is slow/down/throttled
   the inference fails on the provider side, after the point relay can
   recover. Violates the "fail pre-first-byte, no mid-stream surprises"
   contract.
4. **No uniform mechanism** — Gemini (non-Vertex) won't fetch an arbitrary
   bucket at all; you must upload to *Google's* File API.

The sound design keeps the blob inside the **provider's own trust
boundary** (their file API), not relay-hosted URLs.

## Scope

- **Media only.** Text stays inline. Audio/video/image/document blobs
  above a size threshold are candidates.
- **Opt-in, never silent.** Gated by a per-host capability; documented;
  no transparent stripping of bytes the caller thinks reach upstream raw.
- **A pipeline pre-flight concern, not an adapter concern.** Uploading is
  a side-effecting step; translators stay pure (codebase rule 6).

## The credential-scope boundary (the load-bearing constraint)

A `file_id` is scoped to the provider **account/org/project** the API key
belongs to — *not* the model:

| Provider | file_id scope | Retention |
|---|---|---|
| OpenAI | project/org | until deleted |
| Anthropic | workspace/org | provider-defined |
| Gemini | project/key | ~48h (expires) |

Consequences for relay:

- **Model switch within a host → safe** (same credentials, same file
  namespace).
- **Host-to-host failover → breaks** (different provider). Re-upload to
  the new host.
- **Keypool key failover within a host → breaks only if** the pool mixes
  keys from different orgs/projects. A single-org pool (the common case —
  multiple keys for quota headroom) is safe.

So the cache key is `(credential-scope, content-hash) → file_id`, not
`(host, content-hash)`.

## Design: ride the KeyAgent self-heal loop

We don't need to know the operator's credential topology up front. A
stale / cross-scope / expired `file_id` surfaces as a **provider 404,
pre-first-byte** — structurally identical to the `FailureAuth` the
**`pipeline.KeyAgent`** already handles (`docs` / CLAUDE.md "Secret
failover/heal"):

> detect failure → out-of-band re-resolve → retry the same request before
> any bytes flow.

The media cache rides the same loop:

1. Optimistically scope the cache **per-host** for maximum dedup.
2. Pre-flight: for each candidate media part, compute `content-hash`,
   look up `file_id` in kv. Hit → rewrite the part to a reference. Miss →
   upload to the chosen host's file API, store `file_id`, rewrite.
3. If `Adapter.Call` returns a media-ref **404** (stale scope / expired /
   wrong account): re-upload, rewrite, retry — within the existing
   pre-first-byte failover window. Self-healing; the mixed-org edge case
   becomes a non-issue instead of something to detect.

This reuses the failover machinery already in the pipeline rather than
forking a parallel one.

## Components

- **Canonical media part** — a content item that is *either* inline bytes
  *or* a relay-managed handle (`{content_hash, mime, size}`). Adapters
  render it in their vocabulary: a host with file support emits the
  provider file reference; one without inlines the bytes. (Open question
  below: first-class canonical field vs. `extensions`/`provider_data`.)
- **`Spec` capability flag** — per host: "accepts media of type X by
  reference," plus the upload/delete endpoints + auth strategy. Hosts
  without it always inline → no behavior change.
- **Pre-flight uploader stage** — runs *after* shape translation, *before*
  `Adapter.Call`. Owns the content-hash → file_id cache and the upload
  side effect. Keeps the translator pure.
- **kv cache** — `content-hash → file_id`, hash-tagged per the
  `pkg/kv` conventions, **TTL ≤ provider retention** (≤48h for Gemini). A
  miss/expiry just re-uploads; one path covers expiry + scope-miss.

## Worth-it gate

Offload only media **above a size threshold**, because a one-shot single
blob = upload + reference = *two* round-trips = slower than inlining. The
win is purely on **reuse** (resends within a session, or the same file
across requests): first send pays the upload, it's net-positive at ≥2
sends, and the content-hash cache makes every resend free. Gate on
`size > N`; let reuse do the rest.

## Open questions

- **Canonical surface**: is "media by reference" clean enough cross-vendor
  to be a first-class canonical field (like `cache_config`), or does it
  live in `extensions`? Leaning first-class — it's a genuine cross-vendor
  intent, not a vendor quirk.
- **Cache scoping vs. privacy**: per-host dedup means two customers sending
  the same blob to the same provider account upload once. Account-level
  dedup is probably fine; if not, scope per-policy/relay-key at a dedup
  cost. Decide during design.
- **Cleanup**: do we delete uploaded files (OpenAI/Anthropic persist) or
  let TTL/provider-expiry handle it? Gemini self-expires; the others need
  a janitor or we accept accumulation.
- **Failover upload latency**: re-uploading a multi-MB blob on failover
  adds real latency to that (rare, pre-first-byte) path. Acceptable, but
  measure.

## Relationship to the storage path

This shares its core primitive — **content-hash addressing of media** —
with the logging/storage work (`docs/payload-logging.md`, the
content-addressed blob store, "Idea B2"). Both hash media to dedup it; one
references the provider's copy for *inference*, the other stores a single
relay-side copy for *audit*. Build the hashing primitive once.
