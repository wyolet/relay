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

## Status

Early. Architecture is locked; implementation is starting.

## License

TBD.
