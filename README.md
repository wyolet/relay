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

## Status

M5 complete: PG-backed catalog, seed CLI, auto-seed, `/admin/reload`.

## License

TBD.
