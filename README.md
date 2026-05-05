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

## Status

M3 complete: rate limiting, sliding-window-counter, three-meter Reserve/Commit, quota-aware pool selection.

## License

TBD.
