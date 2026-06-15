# relay Helm chart

Production deployment of [Wyolet Relay](https://github.com/wyolet/relay) — a
single self-migrating binary serving the **inference (data) plane** and the
**control plane**, with its **own** bundled PostgreSQL, ClickHouse, and Valkey.

> **Relay owns its data stores.** It never uses the platform's shared "wyolet"
> Postgres. The chart bundles a relay-dedicated PG by default; if you disable it,
> point `external.pgDsn` at a *separate, relay-only* Postgres.

## What's in the chart

| Component | Kind | Purpose | Toggle |
|---|---|---|---|
| relay | Deployment + 2 Services | data plane `:8080` + control plane `:8081` (one binary) | always |
| PostgreSQL | StatefulSet + PVC | config truth; relay self-migrates on boot | `postgresql.enabled` |
| ClickHouse | StatefulSet + PVC | usage + payload events | `clickhouse.enabled` |
| Valkey | StatefulSet + PVC | hot state + rate-limit counters | `valkey.enabled` |
| HPA / PDB | — | scaling + disruption budget | `autoscaling`, `podDisruptionBudget` |
| ServiceMonitor | monitoring.coreos.com | scrape `/metrics` on control port | `serviceMonitor.enabled` |
| Ingress | networking.k8s.io | TLS edges for both planes | `ingress.enabled` |
| NetworkPolicy | — | lock data stores to relay pods | `networkPolicy.enabled` |

## Architecture facts the chart encodes

- **One binary, two planes.** `RELAY_MODE` is `oss`/`cloud`, not a plane split.
  Data plane on `RELAY_PORT` (8080, `/v1/*` + `/healthz`); control plane on
  `RELAY_CONTROL_PORT` (8081, UI/CRUD/`/metrics`/`/version`).
- **Self-migrating.** Relay runs PG migrations on boot under a golang-migrate
  advisory lock, so concurrent replicas are safe — no separate migration Job.
- **Self-seeding.** The lean image bakes the catalog at `/catalog` with
  `RELAY_AUTO_SEED_IF_EMPTY=1`; an empty PG is seeded on first boot.
- **Cluster mode on.** `RELAY_CLUSTER_MODE=on` keeps every pod's in-memory
  snapshot in sync via PG NOTIFY/LISTEN (~1s).
- **Backends.** `RELAY_STATE_BACKEND=redis` → Valkey; `RELAY_EVENTLOG_BACKEND=clickhouse`
  → ClickHouse (`RELAY_CH_DSN`). DSNs are assembled into the Secret.

## Install

```sh
# 1. Generate the security-critical secrets
openssl rand -base64 32                         # -> secrets.masterKey
python3 -c "import secrets;print('sk-wr-'+secrets.token_urlsafe(36))"   # -> secrets.adminToken

# 2. Copy and fill the prod values (use sealed-secrets/SOPS for the secrets)
cp infra/k8s/relay/values-prod.example.yaml values-prod.yaml
$EDITOR values-prod.yaml

# 3. Deploy
helm upgrade --install relay ./infra/k8s/relay \
  -n relay --create-namespace -f values-prod.yaml
```

Required values (template fails fast otherwise): `secrets.masterKey`,
`secrets.adminToken`, and `postgresql.auth.password` /
`clickhouse.auth.password` for the bundled stores — **or** `secrets.existingSecret`
holding `RELAY_MASTER_KEY`, `RELAY_ADMIN_TOKEN`, `RELAY_PG_DSN`, `RELAY_CH_DSN`
(+ `postgres-password` / `clickhouse-password` if bundling those stores).

## Image

Use the **lean** image (`ghcr.io/wyolet/relay:<version>`) — external data stores.
Do **not** use `:standalone` here; that variant bakes a single-node embedded
Postgres for `docker run` demos and will fight the bundled PG.

## Validate before applying

```sh
helm lint infra/k8s/relay -f values-prod.yaml
helm template relay infra/k8s/relay -f values-prod.yaml | kubectl apply --dry-run=client -f -
```

## Known prod caveats (deliberate, single-node defaults)

- **PG/CH are single-node** with a PVC. Fine for a first/dogfood prod; for HA,
  point `external.pgDsn`/`external.chDsn` at managed/replicated stores and set
  the bundled toggles to `false`.
- **Valkey is passwordless** (relay reads only `RELAY_REDIS_ADDR`). Enable
  `networkPolicy` to restrict it to relay pods.
- **OTel traces** aren't wired in relay yet (`RELAY_OTLP_ENDPOINT` is reserved);
  Prometheus metrics + structured logs + `/logs` cover observability today.
- The relay container runs read-only-rootfs; the ClickHouse WAL and temp live on
  emptyDir (`RELAY_EVENTLOG_DIR`). Unacked WAL segments are lost on pod restart —
  acceptable for usage metrics (drop-on-full by design).
