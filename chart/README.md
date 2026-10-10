# relay Helm chart

Production deployment of [Wyolet Relay](https://github.com/wyolet/relay) — a
single self-migrating binary serving the **inference (data) plane** and the
**control plane**, with its **own** bundled PostgreSQL, ClickHouse, and Valkey.

> **Relay owns its data stores.** It never uses a shared cluster Postgres.
> The chart bundles a relay-dedicated PG by default; if you disable it,
> point `external.pgDsn` at a *separate, relay-only* Postgres.

## What's in the chart

| Component | Kind | Purpose | Toggle |
|---|---|---|---|
| relay | Deployment + 2 Services | data plane `:8080` + control plane `:8081` (one binary) | always |
| Migrations | Job (pre-upgrade hook) | `relay migrate up` before the pods roll | `migrations.job.enabled` |
| PostgreSQL | StatefulSet + PVC | config truth | `postgresql.enabled` |
| ClickHouse | StatefulSet + PVC | usage + payload events | `clickhouse.enabled` |
| Valkey | StatefulSet + PVC | hot state + rate-limit counters | `valkey.enabled` |
| HPA / PDB | — | scaling + disruption budget | `autoscaling`, `podDisruptionBudget` |
| ServiceMonitor | monitoring.coreos.com | scrape `/metrics` on control port | `serviceMonitor.enabled` |
| Ingress | networking.k8s.io | TLS edges for both planes | `ingress.enabled` |
| NetworkPolicy | — | lock data stores to relay pods | `networkPolicy.enabled` |

## Architecture facts the chart encodes

- **One binary, two planes.** `RELAY_MODE` is `oss`/`cloud`, not a plane split.
  Data plane on `RELAY_PORT` (8080, `/openai/v1/*`, `/anthropic/v1/*`, canonical `/v1/*`, `/healthz`); control plane on
  `RELAY_CONTROL_PORT` (8081, UI/CRUD/`/metrics`/`/version`).
- **Migrations run once per upgrade.** See [Migrations](#migrations).
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
cp chart/values-prod.example.yaml values-prod.yaml
$EDITOR values-prod.yaml

# 3. Deploy the published chart (version = relay release without the leading v)
helm upgrade --install relay oci://ghcr.io/wyolet/charts/relay --version <version> \
  -n relay --create-namespace -f values-prod.yaml
```

The chart version pins the image: `image.tag` defaults to the chart's
`appVersion`, which each release sets to the same version.

Required values (template fails fast otherwise): `secrets.masterKey`,
`secrets.adminToken`, and `postgresql.auth.password` /
`clickhouse.auth.password` for the bundled stores — **or** `secrets.existingSecret`
holding `RELAY_MASTER_KEY`, `RELAY_ADMIN_TOKEN`, `RELAY_PG_DSN`, `RELAY_CH_DSN`
(+ `postgres-password` / `clickhouse-password` if bundling those stores, and
`RELAY_REDIS_PASSWORD` to give the bundled Valkey a password).

## Migrations

On `helm upgrade`, a pre-upgrade hook Job (`<fullname>-migrate`) runs `relay migrate up` with the relay pods' image, env, Secret and ServiceAccount, after waiting for the bundled Postgres. The pods roll only once it succeeds, and they start with `RELAY_MIGRATE_ON_BOOT=off`, so no pod applies a migration and a pod stopped mid-rollout cannot leave the schema half-recorded. If the Job fails, the upgrade fails and the old pods keep serving; a failed Job is kept for `kubectl logs job/<fullname>-migrate` and replaced by the next upgrade. The Job reads the Secret and ConfigMap as they stand before the upgrade, so change the Postgres DSN or password in an upgrade of its own.

A fresh `helm install` runs no Job — there is no Secret or Postgres yet when pre-install hooks run — and the pods migrate on boot under a golang-migrate advisory lock. `helm upgrade --no-hooks` skips the Job while the pods still start with migrations off; don't combine it with a release that adds migrations.

Argo CD renders the chart with `helm template` and runs the Job as a PreSync hook on every sync, the first one included. Its pods keep migrating on boot (a no-op once the Job has run). With the bundled Postgres, sync the first time with `migrations.job.enabled: false`, since Postgres does not exist yet when PreSync runs, then turn it back on.

Set `migrations.job.enabled: false` to drop the Job and migrate on boot on every install and upgrade.

### Recovering from a dirty schema version

If a migration is interrupted, the schema is recorded as dirty at that version and every later migrate (the Job, or a pod migrating on boot) fails with `Dirty database version N`. To recover:

1. Read the state: `kubectl -n relay exec -it sts/relay-postgresql -- psql -U relay relay -c 'SELECT version, dirty FROM schema_migrations'` (with an external Postgres, run the query there).
2. Open `migrations/postgres/<N>_*.up.sql` at the tag of the relay release you are upgrading to and check that every object it creates or alters (tables, columns, indexes, constraints, functions, triggers) is present in the database. Each file runs in a single transaction, so it is normally all there or none of it.
3. All present: record N as applied with `relay migrate force N`. None present: record the previous version with `relay migrate force <N-1>`. Partly present: repair by hand or restore the backup before forcing anything.
4. Run the upgrade again; the Job applies whatever is still pending.

`relay migrate force` needs only `RELAY_PG_DSN`. With the bundled Postgres, forward it and run the release image locally:

```sh
kubectl -n relay port-forward svc/relay-postgresql 5432:5432 &
docker run --rm --network host -e RELAY_PG_DSN='postgres://relay:<password>@127.0.0.1:5432/relay?sslmode=disable' \
  ghcr.io/wyolet/relay:<version> migrate force <N>
```

## Data store access

- **Valkey requires a password** (`valkey.auth.enabled`, default on). Set
  `valkey.auth.password`, or leave it empty to derive a stable one from
  `secrets.masterKey`. With `secrets.existingSecret`, add `RELAY_REDIS_PASSWORD`
  to that Secret; while the key is missing, Valkey keeps running without a
  password. External Valkey/Redis: `external.redisPassword`.
- **NetworkPolicy is on by default** (`networkPolicy.enabled`): the bundled
  PostgreSQL, ClickHouse, and Valkey accept connections from relay pods only.
  Anything else that reads them (Grafana on ClickHouse, backup jobs) needs a
  `networkPolicy.extraIngress` entry. It has no effect on a CNI that does not
  enforce NetworkPolicy.

Upgrading an install from a chart without these defaults: the upgrade restarts
Valkey with the password and rolls the relay pods; relay pods still on the old
spec lose Valkey access until they are replaced, so expect rate-limit and
session errors for the length of the rollout. With `valkey.persistence`
enabled, sessions and counters survive the Valkey restart. Add `extraIngress` entries for other readers before upgrading, or set
`networkPolicy.enabled: false` / `valkey.auth.enabled: false` to keep the
previous behaviour.

## Image

Use the **lean** image (`ghcr.io/wyolet/relay:<version>`) — external data stores.
Do **not** use `:standalone` here; that variant bakes a single-node embedded
Postgres for `docker run` demos and will fight the bundled PG.

## Validate before applying

```sh
helm lint chart -f values-prod.yaml
helm template relay chart -f values-prod.yaml | kubectl apply --dry-run=client -f -
```

## Known prod caveats (deliberate, single-node defaults)

- **PG/CH are single-node** with a PVC. Fine for a first/dogfood prod; for HA,
  point `external.pgDsn`/`external.chDsn` at managed/replicated stores and set
  the bundled toggles to `false`.
- **OTel traces** aren't wired in relay yet (`RELAY_OTLP_ENDPOINT` is reserved);
  Prometheus metrics + structured logs + `/logs` cover observability today.
- The relay container runs read-only-rootfs; the ClickHouse WAL and temp live on
  emptyDir (`RELAY_EVENTLOG_DIR`). Unacked WAL segments are lost on pod restart —
  acceptable for usage metrics (drop-on-full by design).
