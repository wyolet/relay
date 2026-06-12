# Catalog overlays

User customization of catalog-managed resources that **survives
re-seeding**. An overlay is a user-owned sparse patch on top of a
pristine catalog **template** row; the merged **effective** row is what
the snapshot serves. Un-overridden fields keep tracking the upstream
catalog; overridden fields stay pinned.

## Vocabulary (use these three words, exactly)

- **Template** — the pristine catalog row as shipped/seeded. Lives in
  the entity table (`models`, …). Re-seed and catalog upgrades own it.
- **Overlay** — the user's sparse spec patch. Lives in the `overlays`
  table (`kind`, `resource_id`, `patch` JSONB). The user owns it.
- **Effective** — `merge(template, overlay)`. Exists **only in the
  in-memory snapshot**; never in storage.

("snapshot" was already doubly taken — `model.Snapshot` checkpoints and
`catalog.Snapshot` — hence this vocabulary.)

## Why it exists

Catalog-managed rows are replaced by re-seeds/upgrades. Before
overlays, a user edit to such a row either got clobbered by the next
re-seed or (via the `Dirty` flag) blocked the upstream update entirely —
fork-on-write freezes the whole resource over one changed field. An
overlay freezes *only the touched fields*: the catalog can ship a new
context window, new snapshots, new aliases, and they flow through,
while your custom aliases/family/notes stay.

## The load-time merge (the architectural decision)

The merge happens in `app/catalog` while building the snapshot
(`overlay_apply.go`), **not at write time and not in storage**:

- Re-seed is completely overlay-unaware. It writes template rows and
  walks away; the rebuild re-merges. No re-apply step to forget.
- The template is always recoverable: factory reset = `DELETE` the
  overlay (one API call), and PG is never polluted with merged state.
- A backed-up Postgres is the *entire* customization state — no gitops
  required for self-hosters (though nothing prevents managing overlays
  declaratively later; the merge code wouldn't change).

Consequences to know:

- **Effective state lives only in memory.** `psql` shows templates.
  Debug live state via the snapshot debug endpoint or
  `GET /models/by-id/{id}/overlay` (which returns template + patch +
  effective + quarantine state in one response).
- **Direct-PG readers see templates.** The control CRUD `GET` and
  catalogview read PG: they show the template. The overlay subresource
  is the provenance view. (Folding effective+overrides into catalogview
  is a welcome follow-up.)
- Hot path cost: zero. Merging happens at build/reconcile; requests
  read the pre-merged effective rows like any other.

## Merge semantics

JSON merge patch (RFC 7386) with one deviation:

| patch value | behavior |
|---|---|
| `null` | deletes the field |
| object | recurses (template object) or replaces |
| scalar | replaces |
| array | **replaces wholesale** — except union-whitelisted fields |

**Union whitelist** (hardcoded table in `app/overlay/merge.go`, one
conscious decision per field — deliberately not an annotation system):

- `model.spec.aliases` — union, deduped on the **normalized slug** form
  (the equality the resolver matches on, so `x[1m]` + `x.1m` can't
  sneak past as two entries)
- `model.spec.tags` — union, case-insensitive

Union fields can only **add** relative to the template. A patch cannot
remove a template-shipped alias; per-field replace-mode escalation is
deferred until someone actually needs it. Ordered arrays
(`policy.hostKeyIds` — order is key-selection semantics) and
keyed-object lists (`snapshots`, pricing `rates`) stay replace-wholesale;
per-item merge keys are the strategic-merge swamp we are not entering.

**Not patchable**: `enabled` (snapshot membership is decided on
templates before any merge — a merged-in disable would be half in, half
out), and anything outside `spec` (identity is never patchable; a
name-overriding overlay — a *clone* — is a designed future, see below).

## Validation and quarantine

Two distinct moments:

1. **Write time** (`PUT .../overlay`): the patch must be a non-empty
   JSON object, pass the overlay rules, AND merge into a valid
   effective row *right now* — otherwise **400**. Saving known-bad
   input is not what quarantine is for.
2. **Load time**: a template can change under a once-valid patch
   (re-seed renames a snapshot the patch repoints to, etc.). The merge
   is validated at every build/reconcile; on failure the overlay is
   **quarantined**: the pristine template is served, a warning is
   logged, and `GET .../overlay` reports `quarantined: true` with the
   reason. A bad overlay never takes the model — or the snapshot —
   down, and never half-applies.

## API

```
GET    /models/by-id/{id}/overlay    patch + template + effective + quarantine
PUT    /models/by-id/{id}/overlay    {"patch": {...sparse spec fields...}}
DELETE /models/by-id/{id}/overlay    factory reset
```

The overlay is an **explicitly user-managed patch document** — PUT
replaces the whole patch (no diff-on-write magic; that's a UI-era
follow-up, see below). Writes are gated by the same `governance:model`
edit rule as direct edits, hit PG only, and propagate to every pod via
the table's NOTIFY trigger (~1s) like all catalog writes.

Example — pin a custom alias and a family label:

```
PUT /models/by-id/0190.../overlay
{"patch": {"aliases": ["team-fast"], "family": "custom"}}
```

Re-seed ships `aliases: [claude-x[1m]]` later → effective aliases are
`[claude-x[1m], team-fast]`. Both route.

## Mechanics (for the next person in this code)

- `app/overlay` — domain (`Overlay`, `Validate`), merge engine
  (`MergeSpec`, `EffectiveModel`, union table), sqlc store. Pure; no
  catalog imports.
- `app/catalog/overlay_apply.go` — the ONLY place templates become
  effective: build path (`applyOverlays`, runs after `addModels`,
  before snapshot indexing so aliases/refs index the merged spec) and
  reconcile (`ApplyOverlayUpsert/Delete`, plus `ApplyModelUpsert`
  routing templates through `overlaidModel`).
- The snapshot stashes pristine templates for overlaid models
  (`modelTemplates`) so reconcile re-derives/restores without PG I/O.
  `overlaysByTarget` mirrors the table; an overlay whose target is
  missing is **inert** (no FK — it merges if the target appears).
- NOTIFY: `overlays` has a dedicated trigger (migration 000022)
  because of its composite key — payload `overlay:<op>:<kind>|<id>`;
  the listener splits the id slot. (`validKinds` must list every kind —
  forgetting it is silent event drop; found the hard way.)
- v1 scope: `kind = model` only. The table, store, NOTIFY payload, and
  domain are kind-generic — a new kind is a Strategy entry + an
  `applyOverlays` case + endpoints, no migration.

## Deferred (explicitly, with reasons)

- **Diff-on-write**: generic CRUD `PUT` on a catalog-managed row could
  diff against the template and store the sparse difference (auto-revert
  when a field returns to template value). Deferred so v1 keeps the
  existing CRUD untouched and overlay semantics explicit; natural to add
  when the UI grows an edit-with-provenance flow.
- **Clone-as-overlay**: an overlay that overrides `name` and mints a
  new identity → derived model with separate usage/pricing/grants.
  This is the real Easy Registration story for fine-tunes ("clone base,
  set upstream name, attach pricing"). Same machinery, one extra
  dimension; needs identity-minting rules first.
- **More kinds** (pricing tweaks, host notes): wait for a concrete ask.
- **System-owned override whitelist**: overlays on system rows would
  need a per-kind field whitelist beside the governance owner-tier
  invariants. No use case yet — v1 simply follows `governance:model`.
- **Config-dir overlay YAMLs**: declarative overlays for gitops
  self-hosters, same merge code. Cheap once wanted.
- **Catalogview effective+overrides view** — the natural provenance
  home once the UI consumes it.

## Relationship to `Dirty`

`meta.Dirty` (set by generic CRUD edits) remains the legacy "operator
touched this row, seed must not clobber" signal for *direct* edits.
Overlays are the structured replacement for the catalog-managed case:
prefer an overlay when you want upstream updates to keep flowing.
