# Model aliases

Declared aliases let a Model answer to names that aren't catalog
identities — Claude Code's `claude-fable-5[1m]`, fine-tune ids, operator
nicknames — without weakening the routing invariant that **every routed
request resolves to a real catalog Model**. An alias is a *matcher*, not
a name: it changes how a request finds the model and what wire name goes
upstream, and nothing else.

```yaml
kind: Model
spec:
  snapshots:
    - name: claude-fable-5
  pointer: claude-fable-5
  aliases:
    - claude-fable-5[1m]     # exact alias
    - claude-fable-5[*]      # wildcard pattern (the ONLY place '*' is allowed)
```

## The one rule that makes this safe: lookup keys, never identity

Aliases exist **only** in the resolution index. They are not names:

- `metadata.name` and snapshot names remain the only identities.
- The modelref grammar (`provider/model@host` in policy grants) is
  untouched — you cannot grant or deny *an alias*. Policy authorization
  runs **after** resolution on the real Model ID (`PolicyAllowsCombo`),
  so a grant for `anthropic/claude-fable-5` automatically covers every
  alias of it, and an alias can never widen a grant (an alias to an
  ungranted model still fails with `model_not_allowed`).
- Admin URLs, `/v1/models` listings, and the usage `model` slug all use
  real names. An alias-resolved usage event carries the real
  `model_id`/`model`, the caller's raw string in `requested_model`, and
  `extras.resolved_via = "alias:<declared-alias>"`.

## Resolution precedence

Declared aliases are **last-priority matchers**, consulted only when the
whole real-name index misses:

1. Snapshot names (`gpt-4o-2024-11-20`) and their synthesized forms
   (`provider/name`, `name@host`, `provider/name@host`).
2. Declared **exact** aliases (same synthesized forms).
3. Declared **wildcard** patterns, longest normalized literal prefix
   first.

A real catalog name therefore always shadows an alias, and the exact-hit
hot path pays nothing for the feature (the alias probe only runs on
miss).

## Matching is slug-normalized

Lookup keys normalize through `pkg/slug.From`: lowercase, non-alphanumeric
runs collapse to one dash, edges trimmed. So
`claude-fable-5[1m]` ≡ `claude-fable-5-1m` ≡ `CLAUDE.FABLE.5[1M]` — all
hit the same alias.

Pattern literals normalize with the boundary preserved
(`slug.FromPrefix`): the prefix of `claude-fable-5[*]` becomes
`claude-fable-5-` *with the trailing dash*, so the pattern matches
`claude-fable-5[anything]` but **not** `claude-fable-50-foo`. Suffix
literals mirror this (`ft:*:acme` → prefix `ft-`, suffix `-acme`).

## What goes upstream: the verbatim rule

A matched alias resolves to the model's **Pointer snapshot** for
identity/pricing, but the snapshot's upstream name is NOT what goes on
the wire:

- **Exact alias** → the *declared alias string* goes upstream verbatim,
  regardless of how the caller spelled it (`CLAUDE.FABLE.5[1M]` in,
  `claude-fable-5[1m]` on the wire). Declared strings never contain
  relay addressing (`@host`, `provider/`), so the wire name is always
  clean.
- **Pattern alias** → the *caller's raw request string* goes upstream
  verbatim (`ft:gpt-x:acme:7` in, `ft:gpt-x:acme:7` out).

This is `Plan.UpstreamOverride`, read through `Plan.UpstreamModel()`.
**Codebase rule: wire-name consumers call `Plan.UpstreamModel()`, never
`Snapshot.Upstream()` directly** — the byte-pass body rewrite, the
canonical chain's `Request.Model`, the Gemini URL path, and the batch
runner all do. A future callsite that reaches for `Snapshot.Upstream()`
silently breaks alias verbatim carry; grep before adding one.

## Host pins

- Exact aliases support pins: `claude-fable-5[1m]@anthropic` and the
  `X-WR-Upstream-Host` header both work (pinned forms are synthesized
  into the index like snapshot pins), and the pin never leaks into the
  wire name (the declared string is what goes upstream).
- **Patterns do not support pins** (v1 limitation): a pinned ref skips
  the wildcard scan entirely — the `@host` segment is glued into the
  normalized key and would corrupt the match — so it 404s rather than
  routing with a mangled wire name.

## Validation rules (model.Validate, enforced at CRUD + seed + catalog CI)

- At most one `*` per alias; `*` must not be the first character (this
  also bans bare `*`); the literal prefix must survive normalization.
- Explicit per-alias upstream overrides don't exist: verbatim is the
  only behavior.
- No duplicate aliases within a model, compared on *normalized* form
  (`x[1m]` vs `x.1m` is a duplicate; this also catches 63-char slug
  truncation collisions).
- An exact alias may not normalize to one of the model's own snapshot
  names — real names always win, so the alias would be dead.

## Collisions across models

- Two models declaring the same exact alias: tolerated (same class as
  the multivalued `modelsByName` overlap), resolved by a deterministic
  winner — lexicographic (model slug, alias). The catalog repo's
  validation should keep shipped data collision-free; the relay just
  guarantees determinism.
- Overlapping patterns: longest normalized literal prefix wins, with a
  stable tiebreak. Deterministic across rebuilds and reconciles.

## Registering aliases at runtime

An alias is part of the Model spec, so "registering" one is a normal
model update — `PUT /models/by-id/{id}` with the extended
`spec.aliases` — and the catalog NOTIFY fans it out to every pod within
~1s. No new endpoint, no reload.

## SDK parity

The embedded public catalog (`sdk/catalog/catalog.json`) carries each
model's aliases on its pointer-snapshot binding rows;
`sdk/catalog.Resolve` applies the same last-priority precedence and
pattern rules, so a client's `For(ref)` agrees with the server.

## History note

v1alpha2 *removed* an earlier `aliases` concept — that one was
identity-coupled version addressing, a job snapshots + `pointer` took
over. This reintroduction is a different axis: snapshots are the time
axis (dated checkpoints), aliases are the naming axis (alternate
addressing forms). They are deliberately resolution-only precisely so
they can't grow back into identities.

## Why not arbitrary-model passthrough

The alternative design — a per-policy "accept any model name" escape
hatch with synthetic plans — was rejected for v1: it breaks the
every-request-resolves invariant, creates a permanent second class of
unpriced ghost usage, and taxes every future Plan consumer with an
"unresolved?" branch. The design is parked pending a specific external
signal that the resolution-only model is insufficient.
