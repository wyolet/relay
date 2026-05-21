# Catalog validation

How the public catalog (`wyolet/relay-catalog`) is validated, in three
layers. Each layer catches a different class of problem; CI runs all
three.

## Layers

| Layer | What it catches | Where it lives | When it runs |
|---|---|---|---|
| **Structural** (JSON Schema) | required fields, types, regex/length, enum values, additionalProperties | `schemas/v1alpha2/*.schema.json` (generated from Go types via huma) | editor (yaml-language-server), CI optionally |
| **Per-entity** (`Validate()`) | intra-entity invariants — "host-owned policies must not list hostKeyIds", "valueFrom shape must be valid", etc. | `app/<entity>/*.go` `Validate()` methods | `cmd/catalog-validate` via `manifest.ToX`; relay control API; seed |
| **Graph** | cross-entity refs, owner mismatches, snapshot subset, duplicates, orphans | `app/catalogvalidate/` | `cmd/catalog-validate`; relay seed (planned) |

## In the catalog repo

`wyolet/relay-catalog` has its own `go.mod` and imports
`github.com/wyolet/relay/app/catalogvalidate` directly. CI runs a tiny
Go binary that composes relay's schema-generic graph linter with the
catalog's own [Rule]-based curation conventions.

### The Rule struct (relay-side)

```go
// app/catalogvalidate/rule.go
type Rule struct {
    Name        string   // kebab-case id, used by --skip and docs
    Description string   // one-line human summary
    Severity    Severity // default severity; --strict can promote
    Check       func([]manifest.Document) []Issue
}

func RunRules(rules []Rule, docs []manifest.Document, skip map[string]bool) []Issue
```

Rules are first-class values, not anonymous functions. That gives every
rule a stable handle for ignore flags, surfaces them in `--list`
output, and lets us auto-generate docs from the rule slice.

### Composing in the catalog repo

```go
// catalog-repo/cmd/validate/main.go
package main

import (
    "github.com/wyolet/relay/app/catalogvalidate"
    "github.com/wyolet/relay/app/manifest"

    "github.com/wyolet/relay-catalog/cmd/validate/rules"
)

func main() {
    docs, _ := manifest.LoadDir("data")
    issues := catalogvalidate.ValidateGraph(docs)
    issues = append(issues, catalogvalidate.RunRules(rules.All, docs, skip)...)
    if strict {
        issues = catalogvalidate.Promote(issues)
    }
    // print, exit on errors
}
```

```go
// catalog-repo/cmd/validate/rules/ollama_tags.go
package rules

import (
    "github.com/wyolet/relay/app/catalogvalidate"
    "github.com/wyolet/relay/app/manifest"
)

var All = []catalogvalidate.Rule{
    {
        Name:        "ollama-source-tag",
        Description: "Models with ollama-shaped originalName must carry source-ollama-library tag",
        Severity:    catalogvalidate.SeverityWarning,
        Check:       checkOllamaSourceTag,
    },
    // ... more rules
}
```

Each rule lives in its own file, gets its own unit tests, and is
discoverable via `validate --list` or by reading the `All` slice.

### Promote / strict mode

`Promote(issues)` returns a copy where every warning becomes an error.
Useful for release-prep CI (`validate --strict ./data`) to surface
suppressible curation hints as hard errors before tagging.

## In your editor

Every catalog YAML carries a directive comment pointing at the published
schema:

```yaml
# yaml-language-server: $schema=https://relay-api.wyolet.dev/schemas/v1alpha2/Model.schema.json
apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata:
  name: gpt-x
spec:
  ...
```

VS Code (Red Hat YAML extension), neovim with yaml-language-server, and
IntelliJ all read the directive and give live autocomplete + diagnostics
as you type. Multi-document YAML files (`---` separated) work — each
document validates against the schema of its `kind`.

## In CI

The catalog repo's GitHub workflow:

```yaml
- uses: actions/setup-go@v5
- run: go run ./cmd/validate ./data
```

That single command runs all three layers (structural via parser,
per-entity via `Validate()`, graph via `catalogvalidate.ValidateGraph`).
Exit 1 on errors; warnings are surfaced but don't fail the build.

## Why schemas are committed to the relay repo

- **Single source of truth.** They're generated from the same Go types
  the relay binary uses; drift would mean wire-format incompatibility.
- **Make target enforces it.** `make schemas` regenerates. CI compares
  with the committed copy via `git diff --exit-code schemas/`.
- **Hosted at a stable URL.** Caddy serves them at
  `relay-api.wyolet.dev/schemas/v1alpha2/<Kind>.schema.json`. Editors
  pull them by URL; offline users can run the relay binary locally and
  point editors at `file://` paths.

## Adding a new entity kind

When introducing a new kind (say `Quota`) to the catalog:

1. Add the DTO under `app/manifest/dto.go`.
2. Add `Quota *QuotaDTO` to `manifest.Document`.
3. Append to `kinds` in `cmd/catalog-schemas/main.go`.
4. Add cross-ref checks in `app/catalogvalidate/checks.go` if Quota
   references other kinds.
5. Run `make schemas` and commit the new `schemas/v1alpha2/Quota.schema.json`.
6. Document tag conventions / curation expectations in the catalog repo.
