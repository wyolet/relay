# relay Helm chart

Deploys [Wyolet Relay](https://github.com/wyolet/relay): one binary serving the inference API and the control plane, with its own bundled PostgreSQL, ClickHouse and Valkey by default. The guide to deciding values, exposing the two planes, scaling and monitoring is [Kubernetes](https://docs.wyolet.com/deploy/kubernetes) in the docs; this README is the values reference.

> **Relay owns its data stores.** Point it at stores dedicated to it, never a shared cluster Postgres. If you disable the bundled Postgres, set `external.pgDsn` to a relay-only database.

## What's in the chart

| Component | Kind | Purpose | Toggle |
|---|---|---|---|
| relay | Deployment + 2 Services | inference API (`<fullname>`, port 80) and control plane (`<fullname>-control`, port 8081) | always |
| Migrations | Job (pre-upgrade hook) | `relay migrate up` before the pods roll | `migrations.job.enabled` |
| PostgreSQL | StatefulSet + PVC | configuration | `postgresql.enabled` |
| ClickHouse | StatefulSet + PVC | usage events and captured request bodies | `clickhouse.enabled` |
| Valkey | StatefulSet + PVC | sessions, rate-limit counters, key breaker state | `valkey.enabled` |
| HPA / PDB | autoscaling/v2, policy/v1 | scaling and disruption budget | `autoscaling.enabled`, `podDisruptionBudget.enabled` |
| Ingress | networking.k8s.io/v1 | one rule per plane | `ingress.enabled` |
| Gateway + HTTPRoutes | gateway.networking.k8s.io/v1 | Gateway API alternative to the Ingress | `gateway.enabled` |
| NetworkPolicy | networking.k8s.io/v1 | bundled stores accept relay pods only | `networkPolicy.enabled` |
| ServiceMonitor | monitoring.coreos.com/v1 | scrape `/metrics` on the control port | `serviceMonitor.enabled` |
| VMPodScrape | operator.victoriametrics.com/v1beta1 | the same, for the VictoriaMetrics Operator | `vmPodScrape.enabled` |
| Grafana dashboards | ConfigMaps labeled `grafana_dashboard: "1"` | `dashboards/relay-service.json`, `dashboards/relay-usage-clickhouse.json` | `grafanaDashboards.enabled` |
| Settings | ConfigMap | seeds the `payload-logging` and `usage-logging` settings sections | always |
| Admin user | ConfigMap | a `kind: User` for password sign-in | `auth.adminUser.enabled` |
| catalog-server | Deployment + Service + PDB | serves catalog releases over HTTP | `catalogServer.enabled` |

`<fullname>` is the release name when it contains `relay`, else `<release>-relay`.

## Install

```sh
# 1. Generate the security-critical secrets
openssl rand -base64 32                      # -> secrets.masterKey
echo "sk-wr-$(openssl rand -hex 32)"         # -> secrets.adminToken

# 2. Copy and fill the production values (keep secrets in sealed-secrets, SOPS or an existingSecret)
cp chart/values-prod.example.yaml values-prod.yaml
$EDITOR values-prod.yaml

# 3. Install the published chart (version = relay release without the leading v)
helm upgrade --install relay oci://ghcr.io/wyolet/charts/relay --version <version> \
  -n relay --create-namespace -f values-prod.yaml
```

The chart version pins the image: `image.tag` defaults to the chart's `appVersion`, which each release sets to the same version. Use the lean image (`ghcr.io/wyolet/relay:<version>`); the `:standalone` image embeds its own Postgres and is for `docker run`. Images are built for `linux/amd64` only.

Required values (the template fails otherwise): `secrets.masterKey`, `secrets.adminToken`, and `postgresql.auth.password` / `clickhouse.auth.password` for the bundled stores. Or set `secrets.existingSecret` to a Secret holding `RELAY_MASTER_KEY`, `RELAY_ADMIN_TOKEN`, `RELAY_PG_DSN`, `RELAY_CH_DSN`, plus `postgres-password` / `clickhouse-password` when bundling those stores, `RELAY_ADMIN_PASSWORD` when `auth.adminUser.enabled`, and optionally `RELAY_REDIS_PASSWORD` for Valkey.

Validate before applying:

```sh
helm lint chart -f values-prod.yaml
helm template relay chart -f values-prod.yaml | kubectl apply --dry-run=client -f -
```

## Values

### Relay

| Value | Default | Description |
|---|---|---|
| `replicaCount` | `2` | Replicas when `autoscaling.enabled` is false. |
| `updateStrategy` | `{}` | Deployment strategy; empty keeps `RollingUpdate`. `type: Recreate` is not a substitute for scaling to 0 in a stop-and-start upgrade. |
| `image.repository` | `ghcr.io/wyolet/relay` | Lean image. |
| `image.tag` | `""` | Defaults to the chart's `appVersion`. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `clusterDomain` | `cluster.local` | Cluster DNS domain used to build fully qualified store hostnames in the DSNs. |
| `relay.port` | `8080` | Inference listener port (`RELAY_PORT`). |
| `relay.controlPort` | `8081` | Control listener port (`RELAY_CONTROL_PORT`). |
| `relay.mode` | `oss` | `RELAY_MODE`, read only by the admin UI. |
| `relay.clusterMode` | `"on"` | Sets `RELAY_CLUSTER_MODE`, which has no effect: every pod listens for catalog changes on Postgres regardless. |
| `relay.eventlogBackend` | `clickhouse` | Usage backend used while the `usage-logging` section names none (`RELAY_EVENTLOG_BACKEND`). Set `postgres` when ClickHouse is off. |
| `relay.stateBackend` | `redis` | `RELAY_STATE_BACKEND`. `redis` uses the bundled or external Valkey. |
| `relay.logLevel` | `info` | `RELAY_LOG_LEVEL`. |
| `relay.cookieSecure` | `"true"` | `RELAY_COOKIE_SECURE`. `false` only for plain-HTTP testing. |
| `relay.maxRequestBytes` | `""` | `RELAY_MAX_REQUEST_BYTES`; empty keeps relay's 32 MiB. |
| `relay.maxConns` | `""` | `RELAY_MAX_CONNS`; empty sizes the inference listener's connection cap from `goMemLimit` (5760 at the default `900MiB`). |
| `relay.upstreamDialTimeoutS` | `""` | `RELAY_UPSTREAM_DIAL_TIMEOUT_S`; empty keeps relay's 10 s. |
| `relay.proxyBodyIdleS` | `""` | `RELAY_PROXY_BODY_IDLE_S`; empty keeps relay's 30 seconds, `"0"` disables. |
| `relay.preStopSleepS` | `5` | Seconds a deleted pod keeps serving before relay gets SIGTERM, so the ingress drops the endpoint before the listener closes. Uses the `preStop` sleep action, which needs Kubernetes 1.30+; `0` removes the hook. |
| `relay.shutdownDeadlineS` | `""` | `RELAY_SHUTDOWN_DEADLINE_S`; empty keeps relay's 15 s for in-flight requests after SIGTERM. |
| `relay.terminationGracePeriodSeconds` | `""` | The pod's grace period; empty is `preStopSleepS` + the shutdown deadline + 10 (30 by default). |
| `relay.healthzDeadlineMs` | `"500"` | Sets `RELAY_HEALTHZ_DEADLINE_MS`, which has no effect. |
| `relay.eventlogDir` | `/var/lib/relay/eventlog` | ClickHouse usage write-ahead directory (`RELAY_EVENTLOG_DIR`), on the pod's `emptyDir`. |
| `relay.batchPayloadDir` | `/var/lib/relay/batch-payloads` | Batch payload directory (`RELAY_BATCH_PAYLOAD_DIR`), on the pod's `emptyDir`. See the caveats below. |
| `relay.settingsDir` | `/var/lib/relay/settings` | Where the settings ConfigMap is mounted (`RELAY_SETTINGS_DIR`). |
| `relay.pgMaxConns`, `relay.pgMinConns` | `""` | Postgres pool size per pod; empty keeps relay's 10 / 2. Keep `pgMaxConns` × replicas below Postgres `max_connections` (100 on the bundled one). |
| `relay.redisPoolSize`, `relay.redisMinIdleConns` | `""` | Valkey client pool size and idle floor; empty keeps relay's 25 connections per CPU and no idle floor. |
| `relay.redisPostFlightPoolSize`, `relay.redisPostFlightPoolTimeoutMs`, `relay.redisPostFlightReadTimeoutMs` | `""` | `RELAY_REDIS_POSTFLIGHT_*`: the pool size, pool wait and read bound of the Valkey client that writes after the response; empty keeps relay's 25 per CPU / 1000 / 1000. |
| `relay.redisPostFlightBatchSize`, `relay.redisPostFlightBatchDelayMs`, `relay.redisPostFlightQueueSize` | `""` | `RELAY_REDIS_POSTFLIGHT_BATCH_SIZE`, `_BATCH_DELAY_MS`, `_QUEUE_SIZE`: how many requests' settles share one round trip on that client, how long a batch waits to fill, and how many may wait; empty keeps relay's 64 / 2 / 4096. A queue size of `0` turns batching off. |
| `relay.payloadLogging.enabled` | `true` | Seeds `payload-logging.enabled`. Capture still needs a per-policy opt-in. |
| `relay.payloadLogging.backend` | `clickhouse` | `file`, `s3` or `clickhouse`. `clickhouse` reuses `RELAY_CH_DSN`. |
| `relay.payloadLogging.maxBytes` | `1048576` | Cap per stored body; `0` is unlimited. |
| `relay.payloadLogging.retentionDays` | `30` | Days bodies are kept (ClickHouse); `0` keeps them forever. |
| `relay.payloadLogging.walDir` | `/var/lib/relay/payload-wal` | ClickHouse payload write-ahead directory. |
| `relay.payloadLogging.dedup` | `false` | Store each distinct message, tool list and system prompt once (ClickHouse); pair it with `maxBytes: 0`. |
| `relay.usageLogging.retentionDays` | `0` | Seeds `usage-logging.retentionDays`; `0` keeps events forever. |
| `relay.controlAllowOrigins` | `""` | `RELAY_CONTROL_ALLOW_ORIGINS`, for a UI served from another origin. |
| `relay.inferenceApiUrl` | `""` | `RELAY_INFERENCE_API_URL`; empty derives `https://<inference host>` from `gateway.*` or `ingress.*`. |
| `relay.catalogVersion` | `""` | `RELAY_CATALOG_VERSION`: pin the catalog to a relay-catalog release tag; a change re-seeds at the next boot. |
| `relay.catalogUrl` | `""` | `RELAY_CATALOG_URL`, for a mirror. The archive needs its `.sha256` file next to it. |
| `relay.extraEnv` | `[]` | Extra `name`/`value` env vars for the relay container and the migration Job (for example `RELAY_MAX_INFLIGHT`, `RELAY_PPROF_ADDR`). |
| `resources` | requests 250m / 256Mi, limits 1 CPU / 1Gi | Relay container. |
| `initResources` | requests 10m / 16Mi, limits 100m / 32Mi | The wait-for-stores init containers. |
| `goMemLimit` | `900MiB` | `GOMEMLIMIT`; keep it below the memory limit. |
| `goGC` | `"100"` | `GOGC`. |
| `podAnnotations`, `podLabels` | `{}` | |
| `podSecurityContext`, `securityContext` | non-root 65532, read-only root filesystem, no capabilities | |
| `nodeSelector`, `tolerations`, `affinity` | empty | |
| `topologySpreadConstraints` | spread across `kubernetes.io/hostname` | |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, `""`, `{}` | |
| `service.type` | `ClusterIP` | |
| `service.inferencePort` | `80` | Service port in front of `relay.port`. |
| `service.controlPort` | `8081` | Service port in front of `relay.controlPort`. |
| `service.annotations` | `{}` | |

### Migrations, secrets and sign-in

| Value | Default | Description |
|---|---|---|
| `migrations.job.enabled` | `true` | Run `relay migrate up` in a pre-upgrade Job. |
| `migrations.job.backoffLimit` | `1` | Retries after a failed run. |
| `migrations.job.activeDeadlineSeconds` | `900` | Cap on the Job, including the wait for Postgres. |
| `secrets.existingSecret` | `""` | Use this Secret instead of creating one (keys listed under Install). |
| `secrets.masterKey` | `""` | Base64 of 32 random bytes. Required without `existingSecret`. |
| `secrets.adminToken` | `""` | Admin bearer token. Required without `existingSecret`. |
| `auth.adminUser.enabled` | `false` | Mount a `kind: User` for username/password sign-in. |
| `auth.adminUser.username` | `admin` | |
| `auth.adminUser.email` | `admin@relay.local` | Must be a valid address. |
| `auth.adminUser.password` | `""` | Stored in the Secret as `RELAY_ADMIN_PASSWORD`. With `existingSecret`, put it there instead. |
| `auth.adminUser.configDir` | `/var/lib/relay/users` | `RELAY_CONFIG_DIR` when the admin user is enabled. |

### Data stores

| Value | Default | Description |
|---|---|---|
| `postgresql.enabled` | `true` | Bundled Postgres (`postgres:16-alpine`). |
| `postgresql.auth.username`, `.database` | `relay`, `relay` | |
| `postgresql.auth.password` | `""` | Required when bundled. |
| `postgresql.primary.persistence.enabled`, `.size`, `.storageClass`, `.existingClaim` | `true`, `10Gi`, `""`, `""` | |
| `postgresql.primary.resources` | requests 250m / 512Mi, limits 1 CPU / 1Gi | |
| `clickhouse.enabled` | `true` | Bundled ClickHouse (`clickhouse/clickhouse-server:24.8-alpine`). |
| `clickhouse.auth.username`, `.database` | `relay`, `relay` | |
| `clickhouse.auth.password` | `""` | Required when bundled. |
| `clickhouse.persistence.enabled`, `.size`, `.storageClass` | `true`, `50Gi`, `""` | |
| `clickhouse.resources` | requests 500m / 1Gi, limits 2 CPU / 4Gi | |
| `valkey.enabled` | `true` | Bundled Valkey (`valkey/valkey:8-alpine`). |
| `valkey.auth.enabled` | `true` | Require a password. |
| `valkey.auth.password` | `""` | Empty derives a stable password from `secrets.masterKey`. With `existingSecret`, put `RELAY_REDIS_PASSWORD` there; without it Valkey runs without a password. |
| `valkey.persistence.enabled`, `.size`, `.storageClass` | `true`, `5Gi`, `""` | |
| `valkey.resources` | requests 100m / 128Mi, limits 500m / 512Mi | |
| `external.pgDsn` | `""` | Required when `postgresql.enabled` is false. |
| `external.chDsn` | `""` | ClickHouse DSN when `clickhouse.enabled` is false. |
| `external.redisAddr`, `external.redisPassword` | `""` | Valkey or Redis when `valkey.enabled` is false. |
| `networkPolicy.enabled` | `true` | Bundled stores accept connections from relay pods only. |
| `networkPolicy.extraIngress` | `{}` | Extra ingress rules per store (`postgresql`, `clickhouse`, `valkey`), for readers such as Grafana or backup jobs. |

### Scaling, exposure and monitoring

| Value | Default | Description |
|---|---|---|
| `autoscaling.enabled` | `true` | HorizontalPodAutoscaler. |
| `autoscaling.minReplicas`, `.maxReplicas` | `2`, `10` | |
| `autoscaling.targetCPUUtilizationPercentage` | `70` | |
| `autoscaling.targetMemoryUtilizationPercentage` | `""` | Empty adds no memory target. |
| `podDisruptionBudget.enabled`, `.minAvailable` | `true`, `1` | |
| `ingress.enabled` | `false` | |
| `ingress.className`, `.annotations`, `.tls` | `""`, `{}`, `[]` | Turn off response buffering in the controller; inference responses stream. |
| `ingress.inference.host`, `ingress.control.host` | `""` | One rule per non-empty host. |
| `gateway.enabled` | `false` | Gateway with an HTTPS listener per host, plus HTTPRoutes. Leave `ingress.enabled` off with it. |
| `gateway.className` | `""` | Required when enabled. |
| `gateway.certClusterIssuer` | `""` | cert-manager cluster issuer; one certificate covers both hosts. |
| `gateway.externalDnsTarget` | `""` | external-dns target annotation. |
| `gateway.tlsSecretName` | `""` | Defaults to `<fullname>-tls`. |
| `gateway.httpsPort` | `443` | |
| `gateway.inference.host`, `gateway.control.host` | `""` | At least one is required when enabled. |
| `serviceMonitor.enabled`, `.interval`, `.scrapeTimeout`, `.labels` | `false`, `30s`, `10s`, `{}` | Needs the Prometheus Operator CRDs. |
| `vmPodScrape.enabled`, `.interval`, `.scrapeTimeout`, `.labels` | `false`, `15s`, `10s`, `{}` | Needs the VictoriaMetrics Operator CRDs. |
| `grafanaDashboards.enabled` | `false` | Dashboards as ConfigMaps for a Grafana sidecar. The usage dashboard needs the `grafana-clickhouse-datasource` plugin and network access to ClickHouse. |

### catalog-server

| Value | Default | Description |
|---|---|---|
| `catalogServer.enabled` | `false` | Deploy catalog-server. |
| `catalogServer.replicaCount` | `2` | |
| `catalogServer.image.repository`, `.tag`, `.pullPolicy` | `ghcr.io/wyolet/catalog-server`, `""` (appVersion), `IfNotPresent` | |
| `catalogServer.env` | `[]` | `RELAY_CATALOG_SERVER_*` variables; unset ones keep the binary's defaults. |
| `catalogServer.resources` | requests 50m / 64Mi, limit 256Mi | |
| `catalogServer.tmpSizeLimit` | `256Mi` | `emptyDir` at `/tmp` for the git mirror and build scratch. |
| `catalogServer.podDisruptionBudget.enabled`, `.minAvailable` | `true`, `1` | |
| `catalogServer.service.type`, `.port` | `ClusterIP`, `80` | |
| `catalogServer.ingress.*` | disabled | `className`, `annotations`, `host`, `tls`. |
| `catalogServer.httpRoute.*` | disabled | `parentRefs` (an existing Gateway), `host`. |
| `catalogServer.networkPolicy.enabled`, `.from` | `false`, `[]` | Admit only the listed peers to the server port. |

## Migrations

On `helm upgrade`, a pre-upgrade hook Job (`<fullname>-migrate`) runs `relay migrate up` with the relay pods' image, env, Secret and ServiceAccount, after waiting for the bundled Postgres. The pods roll only once it succeeds, and they start with `RELAY_MIGRATE_ON_BOOT=off`. If the Job fails, the upgrade fails and the old pods keep serving; a failed Job is kept for `kubectl logs job/<fullname>-migrate` and replaced by the next upgrade. The Job reads the Secret and ConfigMap as they stand before the upgrade, so change the Postgres DSN or password in an upgrade of its own.

A fresh `helm install` runs no Job (there is no Secret or Postgres yet when pre-install hooks run), and the pods migrate on boot under an advisory lock. `helm upgrade --no-hooks` skips the Job while the pods still start with migrations off; don't combine it with a release that adds migrations. Argo CD runs the Job as a PreSync hook on every sync, the first one included; with the bundled Postgres, sync the first time with `migrations.job.enabled: false`, then turn it back on. Set `migrations.job.enabled: false` to migrate on boot on every install and upgrade.

## Upgrades

Upgrade with `helm upgrade` and the new `--version`. Procedures for releases that need more, rollback, the migration lock timeout and recovering a dirty schema version are in [Upgrading](https://docs.wyolet.com/concepts/upgrade).

### Stop-and-start upgrades

Some upgrades, such as the one to 0.11.6 (schema 34), must not run old and new pods side by side; scale the Deployment to 0 first. The steps are in [Stop-and-start upgrades](https://docs.wyolet.com/concepts/upgrade#stop-and-start-upgrades).

## Data store access

- **Valkey requires a password** (`valkey.auth.enabled`, default on). Set `valkey.auth.password`, or leave it empty to derive a stable one from `secrets.masterKey`. With `secrets.existingSecret`, add `RELAY_REDIS_PASSWORD` to that Secret; while the key is missing, Valkey keeps running without a password. `scripts/gen-secret.sh` mints the key with the Secret, and `scripts/gen-secret.sh --add-redis-password` adds it to a Secret minted without it. Then restart the relay pods before Valkey: relay sends the password when it connects, which a Valkey without one accepts, so only the Valkey restart interrupts requests. External Valkey or Redis: `external.redisPassword`, or the same Secret key with `secrets.existingSecret`.
- **NetworkPolicy is on by default** (`networkPolicy.enabled`): the bundled PostgreSQL, ClickHouse and Valkey accept connections from relay pods only. Anything else that reads them (Grafana on ClickHouse, backup jobs) needs a `networkPolicy.extraIngress` entry. It has no effect on a CNI that does not enforce NetworkPolicy.

Upgrading an install from a chart without these defaults restarts Valkey with the password and rolls the relay pods; relay pods still on the old spec lose Valkey access until they are replaced, so expect rate-limit and session errors for the length of the rollout. With `valkey.persistence` enabled, sessions and counters survive the Valkey restart. Add `extraIngress` entries for other readers before upgrading, or set `networkPolicy.enabled: false` / `valkey.auth.enabled: false` to keep the previous behaviour.

## catalog-server

`catalogServer.enabled: true` deploys [catalog-server](https://docs.wyolet.com/reference/catalog-server), which serves relay-catalog releases to relays and SDKs over HTTP. It runs `ghcr.io/wyolet/catalog-server`, tagged like relay with the chart's `appVersion`, under its own ServiceAccount with no token. It reads no relay Secret or ConfigMap and needs none of the data stores, and relay's resources render the same with it on or off.

- **Config.** Every setting has a default; override with `RELAY_CATALOG_SERVER_*` variables in `catalogServer.env`. The listen address stays `:8080`.
- **State.** The git mirror and build scratch live on an emptyDir at `/tmp` (`catalogServer.tmpSizeLimit`), so the root filesystem stays read-only. A restarted pod clones again (about a second).
- **Readiness.** `/readyz` turns 200 once the first sync has verified the latest release of every channel; each replica syncs on its own.
- **Egress.** HTTPS to the repository host and its release downloads (for GitHub, `github.com` and the host its release downloads redirect to).
- **Exposure.** `catalogServer.ingress` or `catalogServer.httpRoute` (attached to a Gateway you name in `parentRefs`). The proxy must not compress `/releases/*`: clients check the sha256 of the stored `.gz` bytes.
- **NetworkPolicy.** `catalogServer.networkPolicy` admits only the `from` peers (typically the ingress controller) to the server port.

## Known caveats

- **Bundled Postgres and ClickHouse are single-node**, each with one PVC. For high availability, point `external.pgDsn` / `external.chDsn` at managed or replicated stores and set the bundled toggles to `false`.
- **Settings are seeded once.** The `payload-logging` and `usage-logging` values are written only while those sections have no stored value; after that, change them through the control API.
- **Write-ahead segments are per pod.** The relay container runs with a read-only root filesystem; the ClickHouse write-ahead segments live on an emptyDir, so segments not yet written to ClickHouse are lost when a pod is deleted.
- **Batch with more than one replica.** Batch inputs and results are files in `relay.batchPayloadDir`, on each pod's own emptyDir, while any pod may run a batch item or serve its results. With more than one replica a batch item can fail on a pod that does not hold its input. The chart has no value for mounting shared storage there.
- **No traces.** Relay emits Prometheus metrics, JSON logs and the request log (`/api/logs`), not traces.
