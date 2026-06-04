# Contributing to Wyolet Relay

Thanks for your interest in contributing. Relay is a high-throughput,
self-hostable LLM router written in Go. This guide covers how to build,
test, and submit changes, plus the load-bearing rules every change must
obey.

## Prerequisites

- **Go 1.25+**
- **Docker** (for the integration tests and the local compose stack)
- `make` (the `Makefile` is the entry point for most workflows)

## Getting started

```bash
git clone https://github.com/wyolet/relay
cd relay
cp .env.example .env          # then set RELAY_MASTER_KEY (openssl rand -base64 32)
docker compose up             # bundled Postgres / ClickHouse / Valkey / Jaeger
```

The data plane listens on `:5100`, the control plane on `:5103`. See the
deployment docs under `docs/` for configuration details.

## Build & test

```bash
make build                    # build the relay binary
make test                     # go test ./...
make test-integration         # tag-gated PG/e2e tests (needs Docker)
go vet ./...
```

The repo is a **two-module monorepo**: the server module
(`github.com/wyolet/relay`) and the public, vendorable SDK
(`github.com/wyolet/relay/sdk`). A repo-root `go.work` wires them for
local dev. The dependency direction is **server → sdk, never the
reverse** — the SDK imports nothing from `app/` or `internal/`.

## Repository layout

See the layout map at the top of [`CLAUDE.md`](CLAUDE.md), which doubles
as the architectural orientation doc. In short:

- `app/` — the application: domain entities, composition, HTTP handlers,
  the pipeline.
- `pkg/` — server-internal shared libraries (kv, ratelimit, lifecycle,
  crypto, secret, …).
- `sdk/` — the public client library (canonical protocol, vendor
  adapters, catalog, client).
- `internal/` — composition root / boundary (config, storage).
- `cmd/relay/` — the binary entrypoint and the **only** place vendor
  names appear in code.
- `docs/` — design docs, the roadmap, per-adapter fidelity audits.

## Codebase rules (non-negotiable)

The full set lives in [`docs/canonical-protocol.md`](docs/canonical-protocol.md)
("Codebase rules") and is summarized in `CLAUDE.md`. The ones a PR is
most likely to trip on:

1. **Canonical knows nothing.** `sdk/v1/` imports nothing from `app/`,
   `internal/`, or any vendor adapter.
2. **Vendors import canonical.** Each `sdk/adapters/<vendor>/` implements
   `v1.Translator`; vendor adapters never import each other.
3. **No vendor names in `app/` code.** Dispatch/routing/pipeline must not
   branch on a vendor. Only `cmd/relay/main.go`, catalog data strings,
   URL paths, and error messages may name one.
4. **SDK module purity.** The `sdk/` module stays standalone and
   vendorable; `pkg/` imports nothing from `app/`/`internal/`.
5. **No silent drops.** An adapter must never accept canonical input it
   can't express and discard it — emit it, carry it in
   `provider_data`/`extensions`, annotate the drop with a greppable
   `// canonical: <field> dropped — <why>`, or surface safety-relevant
   signals.

These are enforced by grep checks (see `make lint-rules`) that must hold
on every commit.

## Hot-path discipline

The request path is performance-critical. In particular: no Postgres
calls on the request path (reads come from the in-memory snapshot); token
counts come from the provider response, not relay-side tokenization;
all emits (usage, traces, payloads) are async via bounded channels with
drop-on-full — never block the response.

## Pull request workflow

- **Branch off `main`.** One logical change per PR — don't pile unrelated
  changes onto one branch.
- **Write tests.** New behavior gets unit tests; PG/e2e changes get
  integration coverage. Keep `integration/` compiling — a red build there
  silently disables the whole integration path.
- **Run `make test` + `go vet` + `gofmt`** before pushing. CI runs
  build/test/race/vet plus the codebase-rule grep checks.
- **Keep commits focused** with clear messages. Reference issues where
  relevant.
- **Comments:** default to none. Write a comment only when the *why* is
  non-obvious — a hidden constraint, an invariant, or a workaround.

## Releases & images

CI in this repository runs tests, vet, gofmt, and the codebase-rule checks
only — it does **not** build or publish container images. Official images
(`wyolet/relay:latest` lean, `:standalone` all-in-one, and `:<version>` tags)
are built and published to Docker Hub and GHCR by the maintainers, out of band,
from a `v*` git tag. To build an image yourself, use the `Dockerfile` /
`docker-bake.hcl` at the repo root (`docker buildx bake`), or
`docker compose up --build`.

## Reporting bugs / requesting features

Open an issue using the templates. For security issues, **do not open a
public issue** — see [`SECURITY.md`](SECURITY.md).

## License

By contributing, you agree that your contributions will be licensed under
the project's [AGPL-3.0](LICENSE) license.
