# Relay Operator Runbook

Operational reference for running Wyolet Relay in production.

---

## Table of Contents

1. [Deployment](#1-deployment)
2. [Env-var reference](#2-env-var-reference)
3. [Admin API](#3-admin-api)
4. [Secret storage](#4-secret-storage)
5. [Healthcheck semantics](#5-healthcheck-semantics)
6. [Failure modes](#6-failure-modes)
7. [Debugging recipes](#7-debugging-recipes)
8. [Capacity planning](#8-capacity-planning)
9. [Security checklist](#9-security-checklist)

---

## 1. Deployment

### Prerequisites

| Component | Minimum version | Role |
|---|---|---|
| Go | 1.25+ | Build only — not needed at runtime |
| Postgres | 16+ | Catalog (config source of truth) |
| Redis / Valkey | 7+ | Hot cache + rate-limit counters |
| ClickHouse | 23+ | Usage events and analytics |

Redis and ClickHouse are optional for local development (in-memory and file-backed defaults exist). They are required for production.

### Single binary

```bash
go build -o relay ./cmd/relay

# Run migrations against Postgres first
RELAY_PG_DSN="postgres://relay:relay@localhost:5432/relay?sslmode=disable" \
  ./relay migrate up

# Start the server
RELAY_CATALOG_BACKEND=pg \
RELAY_PG_DSN="postgres://relay:relay@localhost:5432/relay?sslmode=disable" \
RELAY_STATE_BACKEND=redis \
RELAY_REDIS_ADDR=localhost:6379 \
RELAY_EVENTLOG_BACKEND=clickhouse \
RELAY_CH_DSN="clickhouse://relay:relay@localhost:9000/relay" \
RELAY_ADMIN_TOKEN=your-admin-token \
  ./relay
```

### Docker

A `Dockerfile` is at the repo root. Build and run:

```bash
docker build -t relay:local .

docker run -p 8080:8080 \
  -e RELAY_CATALOG_BACKEND=pg \
  -e RELAY_PG_DSN="postgres://relay:relay@pg:5432/relay?sslmode=disable" \
  -e RELAY_STATE_BACKEND=redis \
  -e RELAY_REDIS_ADDR=redis:6379 \
  -e RELAY_EVENTLOG_BACKEND=clickhouse \
  -e RELAY_CH_DSN="clickhouse://relay:relay@ch:9000/relay" \
  -e RELAY_ADMIN_TOKEN=your-admin-token \
  relay:local
```

Run migrations before starting the relay container:

```bash
docker run --rm \
  -e RELAY_PG_DSN="postgres://relay:relay@pg:5432/relay?sslmode=disable" \
  relay:local migrate up
```

### docker-compose (reference stack)

`deploy/compose/` brings up the full 6-service stack: nginx LB, two relay instances, Postgres, Valkey, ClickHouse, and Jaeger.

```bash
make smoke-up      # build images, migrate, wait for health
make smoke-seed    # seed Postgres from deploy/compose/config/
make smoke-down    # tear down + remove volumes
```

Services and host ports:

| Container | Image | Host port | Role |
|---|---|---|---|
| `nginx` | `nginx:alpine` | `8080` | Round-robin LB |
| `relay-a` | local build | `8081` | Relay instance A |
| `relay-b` | local build | `8082` | Relay instance B |
| `postgres` | `postgres:16-alpine` | `5432` | Catalog |
| `valkey` | `valkey/valkey:8-alpine` | — | Rate-limit counters |
| `clickhouse` | `clickhouse/clickhouse-server:23-alpine` | — | Usage events |
| `jaeger` | `jaegertracing/all-in-one` | `16686` (UI), `4317` (OTLP) | Traces |

### Kubernetes sketch

Relay is a stateless binary — run it as a `Deployment`. Use the repo `Dockerfile` to build your image. Supply the env vars from the table in §2 via `env` / `envFrom` in your pod spec. Key points:

- Run `relay migrate up` as an init container (or a pre-deploy Job) before rolling new pods.
- Set liveness probe to `/healthz` (see §3).
- Horizontal scale is the throughput lever — see §6 for per-pod capacity numbers.
- Full Kubernetes manifests are out of scope for this runbook; the compose stack is the reference architecture.

---

## 2. Env-var reference

Boot fails with a non-zero exit code if a required DSN is missing for the configured backend — there is no silent fallback.

### Storage backends

| Var | Default | Required when | Semantics |
|---|---|---|---|
| `RELAY_CATALOG_BACKEND` | `yaml` | always | `yaml` = YAML files under `config/`; `pg` = Postgres-backed catalog. |
| `RELAY_PG_DSN` | _(empty)_ | `RELAY_CATALOG_BACKEND=pg` | Postgres connection string, e.g. `postgres://relay:relay@localhost:5432/relay?sslmode=disable`. |
| `RELAY_STATE_BACKEND` | `memory` | always | `memory` = in-process (single pod, dev only); `redis` = Valkey/Redis for shared rate-limit counters. |
| `RELAY_REDIS_ADDR` | _(empty)_ | `RELAY_STATE_BACKEND=redis` | Redis/Valkey address, e.g. `localhost:6379`. |
| `RELAY_EVENTLOG_BACKEND` | `file` | always | `file` = daily-rotated JSONL files; `clickhouse` = insert into ClickHouse `usage_events` table. |
| `RELAY_CH_DSN` | _(empty)_ | `RELAY_EVENTLOG_BACKEND=clickhouse` | ClickHouse connection string, e.g. `clickhouse://relay:relay@localhost:9000/relay`. |
| `RELAY_CH_RETENTION_DAYS` | `90` | `RELAY_EVENTLOG_BACKEND=clickhouse` | TTL in days for the `usage_events` table partition. Set `0` to disable automatic retention. |
| `RELAY_CONFIG_DIR` | `config` | `RELAY_AUTO_SEED_IF_EMPTY=1` | YAML config directory used by auto-seed on first boot. |

### Observability

| Var | Default | Required when | Semantics |
|---|---|---|---|
| `RELAY_OTLP_ENDPOINT` | _(empty)_ | optional | `host:port` of an OTLP/gRPC collector. Empty = no-op tracer (spans not exported). |
| `RELAY_INSTANCE_ID` | hostname | optional | Per-pod identity string. Appears in every usage event and OTel span. |

### Auth

| Var | Default | Required when | Semantics |
|---|---|---|---|
| `RELAY_ADMIN_TOKEN` | _(empty)_ | optional | Bearer token for admin endpoints. When unset, admin routes are not registered (404). Pass via `X-Relay-Admin-Token` header; falls back to `Authorization: Bearer`. Rotation procedure: see §9. |
| `RELAY_API_KEY` | _(empty)_ | optional | Single inbound API key for caller auth. When unset alongside `RELAY_API_KEYS`, relay runs fail-open (a warning is logged). |
| `RELAY_API_KEYS` | _(empty)_ | optional | Comma-separated list of valid inbound API keys. Takes precedence alongside `RELAY_API_KEY` (both are parsed together). |
| `RELAY_MASTER_KEY` | _(empty)_ | `valueFrom.kind=stored` used in any Secret | 32 bytes base64-encoded; AES-GCM-256 master key for stored-mode secret encryption. Missing → stored-mode writes rejected with 400. Env-mode secrets unaffected. Generate with `relay master-key generate`. |

### Admin / tuning

| Var | Default | Required when | Semantics |
|---|---|---|---|
| `RELAY_HEALTHZ_DEADLINE_MS` | `500` | optional | Per-backend ping timeout for `/healthz` in milliseconds. |
| `RELAY_SHUTDOWN_DEADLINE_S` | `15` | optional | Total graceful shutdown budget in seconds. Covers all drain steps. |
| `RELAY_AUTO_SEED_IF_EMPTY` | _(empty)_ | optional | Set to `1` to seed Postgres from `RELAY_CONFIG_DIR` on first boot if all catalog tables are empty. Subsequent boots with any rows are no-ops. |
| `RELAY_MAX_REQUEST_BYTES` | `2097152` (2 MiB) | optional | Maximum inbound request body size in bytes. Requests exceeding this are rejected with 413 before parsing. |
Rich parsing is no longer an env var — it lives in the `parsing` settings section (`PUT /settings/parsing {"richParsing": false}`), hot-reloaded without a restart. Default `on`: full body parse to a typed `ChatRequest` struct (extracts `model`, `stream`, `user`, `metadata`, `messages` as raw), giving callers body-native attribution and raw `messages` for future enrichment with no deep content parse. Set `richParsing` off only when shaving every microsecond matters and body-level attribution is unused; the minimal path extracts only `model`, `stream`, and `user` and ignores the body's `metadata` field. The raw body is always forwarded byte-equivalent to the upstream provider regardless of this setting.

---

## 3. Admin API

The admin CRUD surface is available when `RELAY_CATALOG_BACKEND=pg` and `RELAY_ADMIN_TOKEN` is set. The full machine-readable spec is at `/openapi.json`; the interactive Swagger UI is at `/docs`.

### Auth model

Every admin endpoint is gated by `X-Relay-Admin-Token`. The middleware checks this header first; if absent it falls back to `Authorization: Bearer`. A missing or wrong token returns **404** — the endpoint existence is obscured.

When caller auth is also active (`RELAY_API_KEY` / `RELAY_API_KEYS`), the caller bearer key **also** must pass (via `Authorization: Bearer`). In this dual-auth configuration:

| Header present | Result |
|---|---|
| Neither | 404 (admin gate fires first) |
| `X-Relay-Admin-Token` correct, no caller key | 401 (caller auth middleware) |
| Both correct | 200 / 201 / 204 |

Without caller auth configured, `Authorization: Bearer` carries the admin token directly (legacy single-header path).

### URL shape

Six resource kinds, all under `/control/`:

| Kind | Path prefix |
|---|---|
| `providers` | `/control/providers` |
| `pools` | `/control/pools` |
| `secrets` | `/control/secrets` |
| `models` | `/control/models` |
| `routes` | `/control/routes` |
| `ratelimits` | `/control/ratelimits` |

Standard CRUD per kind:

| Verb | Path | Action |
|---|---|---|
| `GET` | `/control/{kind}` | List all (returns `{"items":[...]}`) |
| `POST` | `/control/{kind}` | Create (201 on success) |
| `GET` | `/control/{kind}/{name}` | Get one (404 if absent) |
| `PUT` | `/control/{kind}/{name}` | Upsert / update |
| `DELETE` | `/control/{kind}/{name}` | Hard delete (204 on success) |

**Attachments** (rate-limit → resource links) are polymorphic — one endpoint for all parent kinds:

| Verb | Path | Notes |
|---|---|---|
| `GET` | `/control/attachments?parent_kind=Pool&parent_name=my-pool` | `parent_kind` and `parent_name` required |
| `POST` | `/control/attachments` | Body: `{parentKind, parentName, ratelimitName, meter}` |
| `DELETE` | `/control/attachments/{id}` | `id` is `parentKind:parentName:ratelimitName:meter` |

### curl example — create a Provider

```bash
curl -s -X POST http://localhost:8080/control/providers \
  -H "X-Relay-Admin-Token: $RELAY_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "metadata": {"name": "openai"},
    "spec": {"kind": "openai", "baseURL": "https://api.openai.com/v1"}
  }'

# List providers
curl -s http://localhost:8080/control/providers \
  -H "X-Relay-Admin-Token: $RELAY_ADMIN_TOKEN"
```

### Auto-reload

Every successful admin write (create, update, delete) atomically commits to Postgres and then triggers a snapshot reload before responding. Subsequent requests see the new state immediately — no manual `/control/reload` required.

### Error envelope

All 4xx/5xx responses use a structured JSON envelope:

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "validation_failed",
    "message": "Secret \"my-secret\" is referenced by Pool \"my-pool\"; remove the reference first"
  }
}
```

Common `type` values: `invalid_request_error`, `server_error`. Common `code` values: `not_found`, `invalid_body`, `validation_failed`, `master_key_required`, `reload_failed`.

### Pre-validation

Writes are validated against the proposed post-write snapshot before the database transaction commits. A broken reference (e.g. deleting a Secret that is still referenced by a Pool) returns **400** with `code: validation_failed`. The PG transaction rolls back automatically — the catalog is left unchanged. This is the behavior introduced by commit `baadaed` (PER-268): deleting a referenced Secret returns 400, not 500.

### M8 limitations

- **Hard delete**: deletes are permanent; there is no soft-delete or trash.
- **Last-writer-wins**: concurrent writes are not protected by ETags or optimistic locking. ETag / conditional-update support is deferred to a future milestone.

---

## 4. Secret storage

Relay supports two modes for storing provider API keys. Choose based on whether you already have a secret manager.

### Mode 1 — `env` (reference an env var)

```json
{"name": "openai-key-1", "valueFrom": {"kind": "env", "env": "OPENAI_API_KEY"}}
```

Relay stores only the env-var **name** in Postgres — the credential itself lives in your orchestrator's secret store (Vault, SOPS, k8s Sealed Secrets, AWS Secrets Manager, etc.). At runtime Relay reads the named env var. If the env var is absent at startup, the secret resolves to an empty string and any pool referencing it will fail validation.

Best for: teams that already have a secret manager and do not want Relay touching credentials.

### Mode 2 — `stored` (encrypted at rest)

```json
{"name": "openai-key-1", "valueFrom": {"kind": "stored", "value": "sk-..."}}
```

Relay encrypts the literal credential with AES-GCM-256 (keyed by `RELAY_MASTER_KEY`) and stores the ciphertext in Postgres. The cleartext never appears in logs or API responses.

Best for: single-cluster dogfood deployments without an existing secret manager.

### RELAY_MASTER_KEY

| | |
|---|---|
| **Default** | _(empty)_ |
| **Required when** | `valueFrom.kind = stored` is used in any Secret |
| **Semantics** | 32 bytes, base64-encoded. AES-GCM-256 master key for stored-mode encryption. Missing → stored-mode writes rejected with **400** (`code: master_key_required`). Env-mode is unaffected. |

Generate a fresh key:

```bash
relay master-key generate
# emits one line, e.g.:
# 4z3Lk9...base64...==
```

Capture it once, store it in your orchestrator's secret store, pass it at runtime:

```bash
RELAY_MASTER_KEY="$(relay master-key generate)" ./relay
```

### API response shapes

Read responses **never** return cleartext. The response struct has no cleartext field — leakage is structurally impossible.

Env-mode response:

```json
{"name": "openai-key-1", "valueFrom": {"kind": "env", "env": "OPENAI_API_KEY"}}
```

Stored-mode response (masked — provider prefix + last 4 chars):

```json
{"name": "openai-key-1", "valueFrom": {"kind": "stored", "value_masked": "sk-...A1B2"}}
```

Recognized prefixes preserved in the mask: `sk-`, `gsk_`, `xai-`, `ant-`, `hf_`. Unrecognized prefixes show `***...last4`.

### Trust model

| Attacker has | Risk |
|---|---|
| PG dump only | Useless ciphertext — no credentials leaked |
| `RELAY_MASTER_KEY` only | Nothing to decrypt — no PG, no ciphertext |
| Both PG dump + master key | Full compromise — same posture as LiteLLM / OpenRouter |

Keep `RELAY_MASTER_KEY` out of Postgres and out of application logs. Treat it like a root CA private key.

### Master-key rotation

Deferred to a future milestone. Workaround: after rotating `RELAY_MASTER_KEY` in your orchestrator, re-PUT each stored Secret via the API — each write re-encrypts with the new key.

```bash
# Re-encrypt a stored secret with the new master key
curl -s -X PUT http://localhost:8080/control/secrets/openai-key-1 \
  -H "X-Relay-Admin-Token: $RELAY_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "openai-key-1", "valueFrom": {"kind": "stored", "value": "sk-..."}}'
```

---

## 5. Healthcheck semantics

```
GET /healthz
```

Pings all configured backends in parallel within `RELAY_HEALTHZ_DEADLINE_MS` (default 500ms).

### Response shapes

All backends healthy — HTTP 200:

```json
{
  "status": "ok",
  "backends": {
    "catalog": "ok",
    "state": "ok",
    "eventlog": "ok"
  }
}
```

Any backend failed — HTTP 503:

```json
{
  "status": "degraded",
  "backends": {
    "catalog": "ok",
    "state": "error: dial tcp: connection refused",
    "eventlog": "ok"
  }
}
```

### Per-backend status

| Backend | Condition reported as `"ok"` | Reported as `"error: <reason>"` |
|---|---|---|
| `catalog` | Postgres ping succeeds, or backend is `yaml` (unconditionally ok) | Postgres ping times out or returns error |
| `state` | Redis ping succeeds, or backend is `memory` (unconditionally ok) | Redis ping times out or returns error |
| `eventlog` | ClickHouse ping succeeds, or backend is `file` (unconditionally ok) | ClickHouse ping times out or returns error |

In-process backends (`yaml`, `memory`, `file`) have no external dependency and always report `"ok"`.

### LB integration

| Probe type | Recommended path | Threshold |
|---|---|---|
| Liveness | `/healthz` — 200 = live | Fail after 3 consecutive 503s (allows transient blips) |
| Readiness | `/healthz` — 200 = ready to receive traffic | Fail on first 503; remove from LB pool immediately |

Recommended readiness probe:

```yaml
readinessProbe:
  httpGet:
    path: /healthz
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 10
  failureThreshold: 1
livenessProbe:
  httpGet:
    path: /healthz
    port: 8080
  periodSeconds: 15
  failureThreshold: 3
```

---

## 6. Failure modes

### Postgres down

The in-memory snapshot loaded at startup continues to serve all routing decisions. Request traffic is unaffected — Postgres is never on the hot path. `POST /control/reload` fails with 500 (cannot re-read catalog), and `relay seed --apply` fails at connection time. `/healthz` flips to 503 with `"catalog": "error: ..."`. Symptoms: healthcheck 503 with catalog error; all requests continue to succeed using the cached snapshot; reload and seed operations fail.

Check first: `psql $RELAY_PG_DSN -c '\conninfo'` to confirm connectivity. Verify PG container/service health. Inspect relay logs for `configstore(pg) ping` error messages.

### Redis down

Rate-limit `Reserve` calls fail. Current behavior is **fail-closed**: requests that cannot acquire a reservation are rejected with 429 (`pool_out_of_capacity`). This prevents unbounded admission when the counter store is unavailable. Circuit-breaker state is per-pod and forks across instances when Redis is unreachable. `/healthz` flips to 503 with `"state": "error: ..."`. Symptoms: 503 on `/healthz`, 429 on all rate-limited routes; pods may admit at different rates if one recovers before another.

Check first: `redis-cli -h $REDIS_HOST ping`. Verify Valkey/Redis container health. Inspect relay logs for `state(redis)` errors. After Redis recovers, counters reset to zero — enforce a brief cool-down or accept a short burst window.

### ClickHouse down

Usage events are dropped with the `relay_eventlog_dropped_total` Prometheus counter incrementing. Requests continue to succeed normally — eventlog writes are fully async with bounded channels and drop-on-full. OTel traces are unaffected (separate path). `/healthz` flips to 503 with `"eventlog": "error: ..."`. Symptoms: 503 on `/healthz`, `relay_eventlog_dropped_total` rising, requests returning 200 normally, gaps in analytics data.

Check first: `clickhouse-client --host $CH_HOST --query 'SELECT 1'`. Inspect relay logs for eventlog flush errors. Once ClickHouse recovers, in-flight events in the channel buffer are flushed; events dropped while it was down are unrecoverable.

### OTel collector down

Spans are silently dropped by the OTel batch processor. The `relay_otel_dropped_total` Prometheus counter increments. Request traffic, rate limiting, and usage events are completely unaffected. There is no `/healthz` signal for the OTel exporter — its health is visible only via the drop counter and missing traces in Jaeger. Symptoms: `relay_otel_dropped_total` rising, absent or gapped traces in Jaeger, no operator-visible impact on requests.

Check first: verify the collector process (`docker ps | grep jaeger` or equivalent), inspect `relay_otel_dropped_total` in Prometheus. Restart the collector — the batch processor reconnects automatically; in-flight spans in the export queue are lost.

---

## 7. Debugging recipes

### Tail file-backed usage events

```bash
tail -f var/eventlog/$(date +%Y-%m-%d).jsonl | jq
# or if RELAY_EVENTLOG_DIR is set:
tail -f "$RELAY_EVENTLOG_DIR/$(date +%Y-%m-%d).jsonl" | jq
```

### Query recent events from ClickHouse

```bash
clickhouse-client \
  --host localhost \
  --query 'SELECT * FROM usage_events ORDER BY started_at DESC LIMIT 50 FORMAT JSONEachRow' \
  | jq
```

Filter by instance:

```bash
clickhouse-client \
  --host localhost \
  --query "SELECT request_id, model, terminated_by, tokens FROM usage_events WHERE instance_id = 'relay-a' ORDER BY started_at DESC LIMIT 20 FORMAT JSONEachRow"
```

### Inspect Redis rate-limit counters

```bash
redis-cli --scan --pattern 'limit:*'

# Read a specific counter
redis-cli get 'limit:<pool>:<window>'
```

### Verify a Reload took effect

1. Modify your YAML config and apply to Postgres:

```bash
relay seed --from config/ --apply
```

2. Reload both pods:

```bash
# When caller auth (RELAY_API_KEY/RELAY_API_KEYS) is active, pass the caller key
# in Authorization and the admin token in X-Relay-Admin-Token:
curl -s -X POST \
  -H "Authorization: Bearer $RELAY_API_KEY" \
  -H "X-Relay-Admin-Token: $RELAY_ADMIN_TOKEN" \
  http://localhost:8081/control/reload
curl -s -X POST \
  -H "Authorization: Bearer $RELAY_API_KEY" \
  -H "X-Relay-Admin-Token: $RELAY_ADMIN_TOKEN" \
  http://localhost:8082/control/reload

# Without caller auth (RELAY_API_KEY unset), use Authorization for the admin token:
curl -s -X POST -H "Authorization: Bearer $RELAY_ADMIN_TOKEN" \
  http://localhost:8081/control/reload
curl -s -X POST -H "Authorization: Bearer $RELAY_ADMIN_TOKEN" \
  http://localhost:8082/control/reload
```

3. Confirm the change landed in PG:

```sql
SELECT name, updated_at FROM rate_limits ORDER BY updated_at DESC LIMIT 5;
```

4. Send a request that exercises the changed limit and verify the new behavior (e.g., new RPM cap enforces at the expected threshold).

### Read the OpenAPI doc

```bash
curl http://localhost:8080/openapi.json | jq
```

The spec is served at `/openapi.json` (unauthenticated). The interactive Swagger UI is at `/docs`. Both are registered as huma operations and reflect all public and admin endpoints.

### Correlate a request across all signals

Every request gets a `request_id` UUID. Use it to join logs, events, and traces:

```bash
# Grep structured JSON logs
grep '"request_id":"<id>"' /var/log/relay.log | jq

# Query ClickHouse
clickhouse-client --query \
  "SELECT * FROM usage_events WHERE request_id = '<id>' FORMAT JSONEachRow"

# Find in Jaeger: open http://localhost:16686, search by Trace ID
# The trace ID matches the OTel span attached to the same request_id
```

---

## 8. Capacity planning

### RPS per pod

The M5 compose smoke (PER-248) validated correctness at 50–200 RPM against a live stack. Relay's internal overhead per request is measured continuously by the `bench/` directory benchmark (PER-255). Run locally with:

```sh
GOMAXPROCS=2 go test -bench=BenchmarkRelayInternalOverhead -benchtime=10000x -count=1 -run='^$' ./bench/...
```

The bench writes `bench/results.json` with p50/p95/p99 in microseconds. The CI workflow (`.github/workflows/p99.yml`) runs this on every PR and posts a comparison table comment. The bench hard-fails when p99 > 5 ms or p50 > 1 ms. Design targets from CLAUDE.md:

| Metric | Target |
|---|---|
| p50 overhead | <1ms |
| p99 overhead | 5ms (internal SLO), 10ms (public claim) |
| RPS per pod | 5–10k |

### Memory baseline

A Relay binary at idle uses approximately 50–100 MB RSS. Under load, the working set grows with connection count and in-flight request buffers. Allocations on the hot path are minimized via `sync.Pool` buffer reuse.

### GOMEMLIMIT

Set to ~80% of the pod memory limit to give the GC headroom before the OOM killer fires:

```bash
# Example: 512 MiB pod limit → 410 MiB GOMEMLIMIT
GOMEMLIMIT=429496730  # bytes
```

Or use the human-readable form (Go 1.21+):

```bash
GOMEMLIMIT=410MiB
```

### GOGC

| Pod RPS target | Recommended `GOGC` |
|---|---|
| < 1k RPS | `100` (default) — no tuning needed |
| 1k–5k RPS | `100` — monitor GC pause via `relay_gc_pause_ns` if exposed |
| 5k–10k RPS | `50` — more frequent GC, lower peak heap, tighter latency |

```bash
GOGC=50 ./relay
```

### Connection pools

Relay uses `pgxpool` for Postgres and the `rueidis` client for Redis. Default pool sizes are reasonable for most deployments. If you see connection wait latency in traces, tune via DSN parameters:

```
RELAY_PG_DSN="...?pool_max_conns=20&pool_min_conns=2"
```

Redis connection pool size is set by the client library defaults; override via `RELAY_REDIS_ADDR` with a pooled proxy (e.g., Envoy) if needed.

---

## 9. Security checklist

### TLS

Relay listens on plain HTTP (`:8080`). TLS terminates at the load balancer or ingress. Do not expose the relay port directly to the internet. The admin endpoint (`/control/reload`) must be on a private network or behind mTLS/IP allowlist — operator's ingress concern.

### API key rotation

Relay supports multiple inbound keys via `RELAY_API_KEYS` (comma-separated). Zero-downtime rotation procedure:

1. Deploy with both the old and new key in `RELAY_API_KEYS`:
   ```bash
   RELAY_API_KEYS=old-key,new-key
   ```
2. Update clients to send the new key.
3. Once all clients are migrated, deploy with only the new key:
   ```bash
   RELAY_API_KEYS=new-key
   ```

`RELAY_API_KEY` (singular) and `RELAY_API_KEYS` (comma-separated) are both parsed and merged at startup. Either or both may be set.

### Admin token rotation

The admin token is a single bearer secret (`RELAY_ADMIN_TOKEN`). Rotation procedure:

1. Deploy new relay instances with the new token value.
2. Update any automation or scripts that call `/control/reload`.
3. Decommission old instances.

There is no multi-token support for the admin endpoint today — rotation requires a rolling deploy.

### Admin endpoint hardening

- All `/control/*` routes are only registered when `RELAY_ADMIN_TOKEN` is set AND `RELAY_CATALOG_BACKEND=pg`.
- A wrong or missing admin token returns 404 (obscures endpoint existence).
- When caller auth is active (`RELAY_API_KEY`/`RELAY_API_KEYS`), pass the caller bearer key in `Authorization: Bearer` and the admin secret in `X-Relay-Admin-Token`. Without caller auth, use `Authorization: Bearer` for the admin token directly.
- Restrict network access to the admin port via your LB/ingress CIDR allowlist or a private-only VPC subnet.
- `RELAY_MASTER_KEY` must be kept separate from Postgres — if both are compromised together, stored-mode secrets are fully exposed.

### CIDR allowlist / network isolation

Relay does not implement application-layer IP allowlisting. Use one of:

- **Private subnet**: run relay pods in a network that is not internet-routable; expose only via internal LB.
- **Ingress CIDR rules**: restrict inbound traffic to known client CIDR ranges at the LB or Kubernetes NetworkPolicy level.
- **mTLS**: terminate mTLS at the ingress and pass a client-cert header to relay for audit.

The API surface (`/v1/chat/completions`, `/v1/messages`, `/v1/batches`, `/v1/models`, `/healthz`) and the admin surface (`/control/*`) should be on separate listener ports or subdomains when possible, so the admin surface can be allowlisted independently.
