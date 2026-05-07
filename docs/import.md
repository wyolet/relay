# relay catalog import

Import provider/model definitions from an external catalog (currently: LiteLLM) into the Relay config directory or directly into Postgres.

## Quick start

```sh
# Write files to ./config/ (default)
relay catalog import litellm

# Limit to specific providers
relay catalog import litellm --providers anthropic,openai

# Preview without touching disk
relay catalog import litellm --out=-

# Push to Postgres instead of files
relay catalog import litellm --apply
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--out=<dir>` | `config` | Directory to write YAML files into. Use `-` for stdout. Ignored when `--apply` is set. |
| `--apply` | false | Push entities directly to Postgres via the storage layer. Mutually exclusive with `--out` (`--apply` wins). |
| `--mode=<mode>` | `upsert` | How to handle existing files or rows: `upsert` (overwrite), `skip-existing` (leave existing), `overwrite` (alias for upsert). |
| `--providers=<list>` | all | Comma-separated `litellm_provider` values to include. |
| `--models=<regex>` | all | Regex filter on model name. |
| `--source-url=<url>` | LiteLLM default | Override the upstream JSON URL. |
| `--source-file=<path>` | — | Read JSON from a local file instead of fetching over the network. |

## Default mode: write files to disk

Running `relay catalog import litellm` (no flags) writes YAML files under `./config/`:

```
config/providers/
  anthropic/
    provider.yaml
    models/
      claude-sonnet-4-20250514.yaml
      claude-3-5-sonnet-20241022.yaml
      ...
  openai/
    provider.yaml
    models/
      gpt-4o.yaml
      o1.yaml
      ...
```

Each file is a standard Relay resource (`apiVersion: relay.wyolet.dev/v1`, `kind: Model|Provider`).

## Recommended workflow

1. **Import** — write files to `./config/`:
   ```sh
   relay catalog import litellm --providers anthropic,openai
   ```

2. **Review** — check what changed:
   ```sh
   git diff config/
   ```

3. **Edit** — customize specific models (add descriptions, tags, URLs):
   ```sh
   $EDITOR config/providers/anthropic/models/claude-sonnet-4-20250514.yaml
   ```

4. **Commit**:
   ```sh
   git commit -am "import litellm 2026-05"
   ```

5. **Deploy** — the YAML backend picks up files automatically; for the PG backend:
   ```sh
   relay seed
   ```

## Re-import workflow

### Accept all updates (`--mode=upsert`, the default)

Re-run the import and review `git diff`. Accept or revert individual files as needed.

```sh
relay catalog import litellm
git diff config/
git checkout config/providers/anthropic/models/claude-3-haiku-20240307.yaml  # revert one file
git commit -am "import litellm 2026-06"
```

### Only new models (`--mode=skip-existing`)

Only writes files that don't already exist on disk. Operator hand-edits are never overwritten.

```sh
relay catalog import litellm --mode=skip-existing
```

To re-import a specific model (e.g. for updated pricing), delete the file and re-run:

```sh
rm config/providers/anthropic/models/claude-sonnet-4-20250514.yaml
relay catalog import litellm --providers anthropic --models claude-sonnet-4-20250514
git diff config/
git commit -am "update claude-sonnet-4 pricing"
```

## Stdout mode (`--out=-`)

Emits all documents to stdout separated by `---`, providers first, then models, both alphabetically sorted. Useful for piping or review without writing files.

```sh
relay catalog import litellm --providers anthropic --out=- | less
```

## Provenance label

Every imported entity carries `metadata.labels.source: litellm` and `metadata.labels.source_version: <git-sha>`. This lets operators distinguish auto-imported entries from hand-curated ones (or hybrid) in their config tree.

## Filename sanitization

Model names containing `:` or `/` (e.g. `llama3:8b`) are written as `llama3_8b.yaml`. The metadata `name` field inside the file retains the original name unchanged.

## PG apply mode (`--apply`)

Pushes entities directly to Postgres via the storage layer. Requires `RELAY_PG_DSN` to be set.

```sh
RELAY_PG_DSN=postgres://... relay catalog import litellm --apply --mode=skip-existing
```

If both `--apply` and `--out` are passed, `--apply` wins and a warning is emitted:
```
WARN import litellm: --apply set, ignoring --out=<dir>
```
