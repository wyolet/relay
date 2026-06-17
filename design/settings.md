# Settings — runtime config model

Relay's stored config lives in **typed settings sections** (`app/settings`),
one DB row per section (opaque JSONB, shape enforced by a Go type +
`Validate()`). The direction is **minimize env**: anything stored and
tenant/override-able is a settings section, not a `RELAY_*` env var.

## The three config paths

A section's live value is resolved in this order:

1. **Runtime API push** — `PUT /settings/{section}` → `Stores.Settings.Upsert`
   → PG NOTIFY → the in-memory cache updates and any hot-swap **Controller**
   reconciles (sink/reader rebuild, no restart). This is the
   **autoconfigure-without-reboot** path: a bootstrap tool POSTs generated
   config (as JSON) to a running instance and it reconfigures live.
2. **Boot YAML seed** — on boot, `settings.SeedDir` upserts any
   `<section>.yaml` from the settings dir (`RELAY_SETTINGS_DIR`, default
   `<RELAY_CONFIG_DIR>/settings`) that has **no DB row yet**
   (*seed-if-absent* — never clobbers a runtime change or a prior seed). This
   is the **first-boot / airgapped** path; managed deployments use path 1
   instead. A missing dir is a no-op. Unknown section / invalid value = fail
   fast.
3. **Code defaults** — `Section.Defaults()`, when neither a DB row nor a seed
   set a value. The read path is total: every registered section always has a
   value.

YAML is the human/artifact format (what the bootstrap tool generates); the
API wire is JSON. So relay parses YAML only on the seed path.

## What stays in `.env`

Only **bootstrap/base** config — things true *before any tenant config* and
needed to *reach the DB that holds settings*:

- `RELAY_PG_DSN` (the settings store itself), `RELAY_MASTER_KEY`, listen
  ports, `RELAY_CONFIG_DIR`.
- Connection DSNs for backends a settings section *selects* (e.g.
  `RELAY_CH_DSN`) are also bootstrap-tier today — you need a connection before
  you can read settings. (Moving these into settings via `secret.Ref` is a
  follow-up.)
- Legacy env knobs that a settings section now supersedes (e.g.
  `RELAY_EVENTLOG_BACKEND`) are honored only as an **interim fallback** when
  the section is unset; they go away once the section is seeded/configured.

## Hot-swap sections

Sections whose change must take effect without a restart wire a reconcile
seam:

- A **value-applier** (`app/settingswatch.Watcher[T]`) for cheap setters
  (e.g. `parsing`).
- A **Controller** that owns a resource lifecycle for backend swaps
  (`app/payloadlog.Controller`, `app/usagelog.Controller`) — drains + closes
  the old sink, repoints the reader. A backend reroute is a **clean break**:
  new events go to the new store, old data stays where it was (no migration).

## Adding a section

Create `app/settings/<section>.go`: the typed struct with JSON tags, a
`Validate() error`, and a `Register(Section{...})` in `init()` with
`Defaults` + `Decode: decodeAndValidate[...]`. The typed GET/PUT endpoints,
the cache, and the seed pick it up automatically. If it needs hot-swap, add a
Watcher or Controller at the composition root.
