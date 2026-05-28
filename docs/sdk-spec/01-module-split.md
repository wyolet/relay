# Stage 1 — module split (EPHEMERAL)

**PR:** `feat/sdk-module` · **Behavior change:** none. Pure relocation + rewire.

## What moves

Relocate the pure closure into a new module rooted at `sdk/`:

```
sdk/
  go.mod            module github.com/wyolet/relay/sdk   (go 1.25)
  v1/               ← from pkg/relay/v1
  client/           ← from pkg/relay/client
  usage/            ← from pkg/usage
  adapters/
    openai/         ← from pkg/adapters/openai
    anthropic/      ← from pkg/adapters/anthropic
    gemini/         ← from pkg/adapters/gemini
```

Confirmed closure (`go list -deps ./pkg/relay/client | grep wyolet/relay`):
`usage`, `relay/v1`, `adapters/{openai,anthropic,gemini}`, `relay/client`.
Nothing else. **Do not move `pkg/kv`** (pulls go-redis) or any other `pkg/*`.

## Import-path rewrite

Old → new for every reference repo-wide (server module + within sdk):

```
github.com/wyolet/relay/pkg/relay/v1        → github.com/wyolet/relay/sdk/v1
github.com/wyolet/relay/pkg/relay/client    → github.com/wyolet/relay/sdk/client
github.com/wyolet/relay/pkg/usage           → github.com/wyolet/relay/sdk/usage
github.com/wyolet/relay/pkg/adapters/openai → github.com/wyolet/relay/sdk/adapters/openai
github.com/wyolet/relay/pkg/adapters/anthropic → .../sdk/adapters/anthropic
github.com/wyolet/relay/pkg/adapters/gemini → .../sdk/adapters/gemini
```

Find sites:
```
grep -rl 'pkg/relay/v1\|pkg/relay/client\|pkg/usage\|pkg/adapters/' --include='*.go' .
```
~16 (v1) + 9 (adapters) + 27 (usage) references; ~18 outside the moved subtree
(server `app/`, `cmd/`). Mechanical — safe to do with a scripted replace, then
`gofmt`. Subagent-able (model: sonnet) but verify the diff.

## Module wiring

- `sdk/go.mod`: `module github.com/wyolet/relay/sdk`, `go 1.25`, require only
  what the closure needs (`coder/websocket`; transitively `golang.org/x/*` via
  net/http are stdlib-vendored, not requires).
- Server `go.mod`: add `require github.com/wyolet/relay/sdk v0.0.0` +
  `replace github.com/wyolet/relay/sdk => ./sdk` for in-repo dev. (Tagging
  strategy `sdk/vX.Y.Z` is a release concern, deferred to first publish.)
- Repo-root `go.work`:
  ```
  go 1.25
  use .
  use ./sdk
  ```
- Move any third-party requires that the moved packages need from the root
  `go.mod` into `sdk/go.mod`; tidy both (`go mod tidy` in each, or
  `go work sync`).

## Acceptance gates

- `go build ./...` and `cd sdk && go build ./...` both green.
- `go test ./...` (both modules) pass.
- `go list -deps ./sdk/client | grep 'wyolet/relay/app\|wyolet/relay/internal'`
  → empty (purity holds).
- `cd sdk && go list -deps ./... | grep -i 'pgx\|redis\|clickhouse'` → empty.
- Codebase-rule greps still pass:
  - rule 4: `grep -rE "openai|anthropic" app/ --include="*.go"` → only catalog
    strings / errors / URLs / `cmd/relay/main.go` (unchanged).
  - rule 10: no `sdk/**` file imports `app/` or `internal/`.
- No `.go` content diff beyond import lines + package-path moves.

## Watch-outs

- `cmd/relay/main.go` builds `adapter.Spec` literals from the vendor packages —
  its imports rewrite to `sdk/adapters/*`. The `app/adapter` framework stays in
  the server module; only the vendor translator packages move.
- Any `//go:embed` or testdata under the moved packages travels with them.
- `go.work` must NOT be committed in a way that breaks consumers — it only
  affects in-repo dev. The `replace` in server `go.mod` is what matters for the
  monorepo build; document that `go.work` is dev-only.
