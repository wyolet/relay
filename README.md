# Wyolet Relay

High-throughput LLM router in Go. Self-hostable, k8s-native, BYO provider keys.

A faster, infra-grade alternative to OpenRouter and LiteLLM for teams running millions of LLM requests per day.

## What it does

- **Unified API** in front of OpenAI, Anthropic, Gemini, Bedrock, and self-hosted models
- **API key pooling** — failover and load-balanced multi-key concurrency to multiply effective rate limits
- **Batch as a primitive** — fire-and-forget bulk submissions that work for any provider, including ones without native batch APIs. Webhook on completion or poll for status.
- **Smart routing** — named Routes with declarative policies (`cheapest`, `fastest`, `cheapest-with-tools`, custom) instead of per-request DSL
- **Built-in observability** — OpenTelemetry traces, Prometheus metrics, ClickHouse-backed usage analytics, decision traces explaining every routing choice
- **Optional payload capture** to S3-compatible storage for replay, eval, and debugging

## Performance targets

| | p50 | p99 | RPS / pod |
|---|---|---|---|
| Internal SLO | <1ms overhead | 5ms overhead | 5–10k |
| Public claim | — | 10ms overhead | — |

Failover-time is treated as a co-equal SLO with overhead.

## Architecture

Three subsystems share one spine (domain model + storage):

```
                  ┌─────────────────────────┐
                  │   DOMAIN & STORAGE      │  ← the spine
                  │   PG + Redis + CH + S3  │
                  └─────────────────────────┘
                    ▲          ▲          ▲
   ┌─────────────────┐ ┌──────────────┐ ┌──────────────┐
   │  Realtime DP    │ │ Batch workers│ │ Control plane│
   │  HTTP/JSON edge │ │ Queue-driven │ │ Admin API    │
   └─────────────────┘ └──────────────┘ └──────────────┘
```

- **Postgres** — config source of truth (orgs, projects, routes, keys)
- **Redis** — hot cache + counters (rate limits, key health, quotas)
- **ClickHouse** — usage events and analytics
- **S3** — opt-in request/response payload storage

The control plane owns truth and fans out config changes via pub/sub. The data plane is stateless and **never** touches Postgres on the hot path.

## API surface

- `POST /v1/chat/completions` — OpenAI-shaped passthrough
- `POST /v1/messages` — Anthropic-shaped passthrough
- `POST /v1/batches` — Relay-native batch
- `X-Relay-*` headers carry control metadata (route, fallback override, budget scope, trace context)

## M2 features (current)

- YAML-file config store (`config/providers/<provider>/...`)
- Multi-provider routing: Ollama (dev) + OpenAI (production), extensible to any provider
- API key pooling with env-var secret refs
- `/v1/models` model listing
- `.env` auto-loader for local development (no `source .env` required)

## Configuration

Config lives under `config/providers/<provider>/`:

```
config/providers/
  ollama/
    provider.yaml   # Provider kind + base URL
    pool.yaml       # Pool listing secrets
    secrets/        # One file per API key
    models/         # One file per model
  openai/
    provider.yaml
    pool.yaml
    secrets/openai-key-1.yaml
    models/gpt-4o-mini.yaml
```

Secret values are resolved from env vars at startup:

```yaml
# secrets/openai-key-1.yaml
spec:
  valueFrom:
    env: OPENAI_API_KEY
```

For local development, create a `.env` file in the repo root:

```
OPENAI_API_KEY=sk-...
```

The relay reads it on startup and does **not** override values already in the environment, so CI/production env vars always take precedence.

## Quick start

```bash
# Dev (Ollama, no key needed)
go run ./cmd/relay

# With OpenAI
OPENAI_API_KEY=sk-... go run ./cmd/relay
# or put it in .env and just:
go run ./cmd/relay

curl localhost:8080/v1/models
curl localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

## Rate limiting (M3)

Rate limits are first-class YAML resources. A `RateLimit` is a generic budget (`strategy + window + amount`); attachments on `Secret` / `Pool` / `Model` declare what to count via a `meter` (`requests` / `tokens` / `concurrency`):

```yaml
# rate-limit definition
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata: { name: gpt4-rpm }
spec:
  strategy: sliding-window
  window: 1m
  amount: 500

# attachment on a Model
spec:
  rateLimits:
    - { ref: gpt4-rpm, meter: requests }
    - { ref: gpt4-tpm, meter: tokens }
    - { ref: gpt4-conc, meter: concurrency }
```

Enforcement is two-phase: **Reserve** pre-flight (requests + concurrency + token-budget peek) and **Commit** post-call (token credit from upstream usage block, concurrency decrement). Pool selection is quota-aware: secrets with more remaining headroom get picked more often.

Exceeding a budget returns a 429 with an OpenAI-shape envelope:

```json
{"error":{"message":"rate limit exceeded: requests","type":"rate_limit_exceeded","code":"rpm_exceeded"}}
```

Codes: `rpm_exceeded`, `tpm_exceeded`, `concurrency_exceeded`, `pool_out_of_capacity`. `Retry-After` header included.

## Caller attribution via X-Relay-Metadata

Attach arbitrary key=value pairs to every request for cost attribution, per-tenant dashboards, and audit logs.

**Format:** comma-separated `k=v` pairs, whitespace-tolerant.

| Limit | Value |
|---|---|
| Max pairs | 16 |
| Max key length | 64 chars (`[a-zA-Z0-9_.-]`) |
| Max value length | 256 chars (printable ASCII, no `,` or `=`) |

Any single violation drops the **entire** header silently — the request still succeeds and is routed normally. A debug log line is emitted and `relay_metadata_rejected_total` increments. No error is returned to the caller.

The header is stripped before forwarding — it never reaches OpenAI, Ollama, or any upstream.

**curl:**

```bash
curl localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Relay-Metadata: customer=acme,env=prod' \
  -d '{"model":"gemma4:31b","messages":[{"role":"user","content":"hi"}]}'
```

**Python OpenAI SDK:**

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="unused",
    default_headers={"X-Relay-Metadata": "customer=acme,env=prod"},
)
resp = client.chat.completions.create(
    model="gemma4:31b",
    messages=[{"role": "user", "content": "hi"}],
)
```

Attribution pairs appear in every JSONL event under `attribution` and as flattened `relay.attr.<key>` tags on the OTel span.

## Observability

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `RELAY_OTLP_ENDPOINT` | _(empty)_ | `host:port` of an OTLP/gRPC collector. Empty = no-op tracer (no spans exported). |
| `RELAY_INSTANCE_ID` | hostname | Identifies the relay pod/process in events and spans. |
| `RELAY_EVENTLOG_DIR` | `./events` | Directory for daily-rotated JSONL event files. |

**Local Jaeger:**

```bash
docker run -d --rm --name jaeger \
  -p 4317:4317 -p 16686:16686 \
  jaegertracing/all-in-one:latest

RELAY_OTLP_ENDPOINT=localhost:4317 go run ./cmd/relay
# open http://localhost:16686, service = relay
```

### Event schema

Each request appends one JSON line to `$RELAY_EVENTLOG_DIR/<date>.jsonl`. Top-level fields:

| Field | Type | Description |
|---|---|---|
| `event_version` | int | Schema version (currently `1`) |
| `request_id` | string | UUID per request |
| `model` | string | Model name as sent by caller |
| `provider` | string | Provider kind (`ollama`, `openai`, …) |
| `pool` | string | Key pool name |
| `secret_hash` | string | First 12 hex chars of sha256 of the winning key |
| `terminated_by` | string | `clean` \| `client_cancel` \| `upstream_error` \| `upstream_timeout` \| `rate_limited` \| `pool_exhausted` \| `relay_error` |
| `tokens` | object | `prompt`, `completion`, `total`, `cached` (int64) |
| `attempts` | array | Per-attempt outcome, HTTP status, latency |
| `attribution` | object | Key=value pairs from `X-Relay-Metadata` (absent if header missing/rejected) |
| `metrics` | object | Pipeline latency stamps in milliseconds (`pre_upstream_ms`, `ttfb_ms`, `upstream_ms`, `total_ms`) |
| `instance_id` | string | Value of `RELAY_INSTANCE_ID` or hostname |
| `relay_version` | string | Go module version or `dev` |
| `started_at` | string | RFC3339Nano UTC |
| `ended_at` | string | RFC3339Nano UTC |

Example event (abbreviated):

```json
{
  "event_version": 1,
  "request_id": "01jwabcdef",
  "model": "gemma4:31b",
  "provider": "ollama",
  "pool": "ollama-pool",
  "secret_hash": "a3f9c12b4e87",
  "terminated_by": "clean",
  "tokens": {"prompt": 12, "completion": 48, "total": 60, "cached": 0},
  "attribution": {"customer": "acme", "env": "prod"},
  "metrics": {"pre_upstream_ms": 1, "ttfb_ms": 210, "upstream_ms": 890, "total_ms": 891},
  "instance_id": "relay-pod-0",
  "relay_version": "dev",
  "started_at": "2026-05-05T10:00:00.000Z",
  "ended_at": "2026-05-05T10:00:00.891Z"
}
```

### Drop counters

| Metric | Meaning |
|---|---|
| `relay_eventlog_dropped_total` | Events that could not be written to the JSONL file (disk full, rotation error). Requests still complete; data is lost. |
| `relay_otel_dropped_total` | OTel spans dropped because the batch processor queue was full or the export RPC failed. Indicates collector backpressure. |
| `relay_metadata_rejected_total` | `X-Relay-Metadata` headers silently dropped due to format violations. Labels: `reason=oversize`, `reason=bad_charset`, `reason=malformed`. Request still succeeds with no attribution. |

## Operator: seeding Postgres from YAML

When `RELAY_CATALOG_BACKEND=pg`, use `relay seed` to populate Postgres from an existing YAML config directory.

### Dry-run (no writes)

```bash
RELAY_PG_DSN="postgres://relay:relay@localhost:5432/relay?sslmode=disable" \
  relay seed --from config/
```

Prints a grouped diff (`+` create, `~` update, `-` delete) without touching the database. Exit 0.

### Apply (idempotent upsert)

```bash
relay seed --from config/ --apply
# or pass DSN inline
relay seed --from config/ --apply --dsn "postgres://..."
```

Runs a single `BEGIN … COMMIT` transaction. Validation runs before the transaction opens — a broken YAML directory exits non-zero and never touches the database. Running twice is a no-op.

### Auto-seed on first boot

Set `RELAY_AUTO_SEED_IF_EMPTY=1` when using the PG backend. On startup, if every catalog table is empty, Relay seeds from `RELAY_CONFIG_DIR` (default: `config/`) and logs:

```
auto-seed: applied N rows from <dir>
```

Subsequent boots that find any rows in any table are no-ops.

| Variable | Default | Description |
|---|---|---|
| `RELAY_AUTO_SEED_IF_EMPTY` | _(empty)_ | Set to `1` to enable auto-seed on boot (PG backend only). |
| `RELAY_CONFIG_DIR` | `config` | YAML config directory used by auto-seed. |

## Operator: admin reload endpoint

`POST /admin/reload` tells the running relay to re-read its catalog from Postgres and swap the in-memory snapshot atomically.

The endpoint is **only registered** when `RELAY_ADMIN_TOKEN` is set. When the env var is absent, the route does not exist (404 from the default handler — no endpoint discovery).

| Auth result | Response |
|---|---|
| Missing / wrong token | 404 (obscures endpoint existence) |
| Correct token | 200 (empty body) |
| Reload error | 500 (JSON error envelope) |

```bash
export RELAY_ADMIN_TOKEN=my-secret-token

curl -X POST http://localhost:8080/admin/reload \
  -H "Authorization: Bearer my-secret-token"
```

Every call is logged at INFO with caller IP and request ID. The endpoint is unmounted when `RELAY_CATALOG_BACKEND` is not `pg` (no PGStore to reload).

## Operator: healthcheck

`GET /healthz` pings every configured backend in parallel and returns:

| Condition | HTTP | Body |
|---|---|---|
| All backends healthy | 200 | `{"status":"ok","backends":{"catalog":"ok","state":"ok","eventlog":"ok"}}` |
| Any backend failed | 503 | `{"status":"degraded","backends":{"catalog":"ok","state":"error: <reason>","eventlog":"ok"}}` |

Backends configured as `memory`/`file`/`yaml` report `"ok"` unconditionally (no external dep to ping). Only `pg`, `redis`, and `clickhouse` backends are pinged.

Per-check deadline: 500ms by default, configurable via `RELAY_HEALTHZ_DEADLINE_MS`.

## Operator: graceful shutdown

Send `SIGTERM` or `SIGINT`. The relay drains in order:

1. Stop accepting new HTTP requests (up to 10s)
2. `usage.Shutdown` — drain OTel batch processor (up to 5s)
3. `eventlog.Close` — flush pending ClickHouse inserts (up to 8s)
4. `state.Close` — drain in-flight Lua scripts
5. `configstore.Close` — close pgxpool

Total wall time is capped by `RELAY_SHUTDOWN_DEADLINE_S` (default `15`).

## Operator: backend env vars

| Variable | Default | Description |
|---|---|---|
| `RELAY_CATALOG_BACKEND` | `yaml` | `yaml` or `pg`. Requires `RELAY_PG_DSN` when `pg`. |
| `RELAY_STATE_BACKEND` | `memory` | `memory` or `redis`. Requires `RELAY_REDIS_ADDR` when `redis`. |
| `RELAY_EVENTLOG_BACKEND` | `file` | `file` or `clickhouse`. Requires `RELAY_CH_DSN` when `clickhouse`. |
| `RELAY_HEALTHZ_DEADLINE_MS` | `500` | Per-backend ping deadline in milliseconds. |
| `RELAY_SHUTDOWN_DEADLINE_S` | `15` | Total graceful shutdown budget in seconds. |

Boot fails with a non-zero exit code if a required DSN is missing or invalid for the configured backend — no silent fallback to in-memory.

## Local production-shape deployment

`deploy/compose/` brings up a full stack: two relay instances behind nginx, backed by Postgres, Valkey (Redis-compatible), ClickHouse, and Jaeger.

### Quick start

```bash
make smoke-up      # build images, start stack, migrate, wait for health
make smoke-seed    # seed Postgres from deploy/compose/config/
make smoke-down    # tear down, remove volumes
```

`smoke-up` automatically runs `smoke-migrate` before starting the relay instances.

### What's running

| Container | Image | Host port | Role |
|---|---|---|---|
| `nginx` | `nginx:alpine` | `8080` | Round-robin LB in front of relay-a and relay-b |
| `relay-a` | local build | `8081` | Relay instance A |
| `relay-b` | local build | `8082` | Relay instance B |
| `postgres` | `postgres:16-alpine` | `5432` | Catalog (config truth) |
| `valkey` | `valkey/valkey:8-alpine` | — | Rate-limit counters + key health |
| `clickhouse` | `clickhouse/clickhouse-server:23-alpine` | — | Usage events |
| `jaeger` | `jaegertracing/all-in-one` | `16686` (UI), `4317` (OTLP) | Traces |

### Env-var matrix (relay instances)

| Variable | Value in compose | Description |
|---|---|---|
| `RELAY_CATALOG_BACKEND` | `pg` | Postgres-backed catalog |
| `RELAY_PG_DSN` | `postgres://relay:relay@postgres:5432/relay?sslmode=disable` | PG connection |
| `RELAY_STATE_BACKEND` | `redis` | Valkey for counters |
| `RELAY_REDIS_ADDR` | `valkey:6379` | Valkey address |
| `RELAY_EVENTLOG_BACKEND` | `clickhouse` | CH for usage events |
| `RELAY_CH_DSN` | `clickhouse://relay:relay@clickhouse:9000/relay` | CH connection |
| `RELAY_OTLP_ENDPOINT` | `jaeger:4317` | OTLP gRPC collector |
| `RELAY_ADMIN_TOKEN` | `smoke-admin-token` | Bearer token for `/admin/reload` |
| `RELAY_INSTANCE_ID` | `relay-a` / `relay-b` | Per-pod identity in events and spans |
| `RELAY_AUTO_SEED_IF_EMPTY` | `1` (relay-a only) | Seeds catalog from `/config` on first boot |

### Fixture catalog

`deploy/compose/config/` contains a minimal catalog: one Provider (Ollama at `host.docker.internal:11434`), one Pool, one Model (`smoke-model`) with a 60 RPM rate limit, and a default Route. Edit the YAML, run `make smoke-seed`, then `POST /admin/reload` to both pods to apply changes without restart.

```bash
# Admin reload
curl -X POST -H "Authorization: Bearer smoke-admin-token" http://localhost:8081/admin/reload
curl -X POST -H "Authorization: Bearer smoke-admin-token" http://localhost:8082/admin/reload
```

### Ports summary

- `http://localhost:8080` — nginx LB (use this for API traffic)
- `http://localhost:8081/healthz` — relay-a direct
- `http://localhost:8082/healthz` — relay-b direct
- `http://localhost:16686` — Jaeger UI
- `localhost:5432` — Postgres (for `psql` / migrations)

## Documentation

| Doc | Contents |
|---|---|
| [docs/runbook.md](docs/runbook.md) | Operator reference: deployment, env-var table, healthcheck semantics, failure modes, debugging recipes, capacity planning, security checklist |

## Status

M5 complete: PG-backed catalog, seed CLI, auto-seed, `/admin/reload`, `/healthz`, graceful shutdown, OTel storage resource attrs. docker-compose smoke stack (PER-248).

## License

TBD.
