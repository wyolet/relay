# Cluster Mode

## The staleness problem

Each Relay pod keeps an in-memory snapshot of the catalog (providers, pools, models, routes, rate-limits, secrets). Reads from the hot path are served from that snapshot; Postgres is never touched on the request path.

In a single-pod deployment this is fine: admin writes call `Reload()` synchronously after every mutation, so the local snapshot is always fresh.

In a multi-pod deployment, writes land on one pod and the other pods' snapshots stay stale until their next poll or restart. Cluster mode solves this.

## What cluster mode does

**Producer (always-on):** every catalog write in `internal/storage/catalog.go` and `internal/storage/secrets.go` calls `pg_notify('relay_catalog', '')` inside the same execution context as the data write (pool connection or transaction). If no listeners exist the message is silently dropped at commit. Cost is microseconds.

**Consumer (gated by `RELAY_CLUSTER_MODE=on`):** a background goroutine opens a single dedicated `*pgx.Conn` (not from the pool — `LISTEN` holds the connection), issues `LISTEN relay_catalog`, and calls `pgStoreB.Reload()` for every notification. Typically the notification arrives within a few milliseconds of the originating commit; the full reload (snapshot swap) adds another few tens of milliseconds, for a total fanout latency well under 100ms in normal conditions.

The NOTIFY payload is empty. The watcher always does a full snapshot reload — there is no partial-update protocol.

## Enabling cluster mode

Set the environment variable before starting each pod:

```
RELAY_CLUSTER_MODE=on
```

Default is `off`. Single-pod deployments are entirely unchanged when the flag is off — no watcher goroutine, no LISTEN connection, no overhead.

`RELAY_CATALOG_BACKEND=pg` is also required (cluster mode only applies to the PG backend). If `RELAY_CATALOG_BACKEND` is not `pg`, the watcher is not started even when `RELAY_CLUSTER_MODE=on`.

## When to enable

Enable cluster mode whenever you run more than one Relay pod against the same Postgres instance. If you use a single pod (even with a Redis state backend), leave it off.

## What is NOT yet behind this flag

Future work that will also be gated by `RELAY_CLUSTER_MODE`:

- Leader election (e.g. for batch workers that must run on exactly one pod).
- Redis Cluster client mode (will switch from standalone to cluster topology when enabled).

Those are deferred; the current implementation is limited to PG NOTIFY/LISTEN catalog fanout.

## Failure modes and operations

**Watcher connection drops:** the goroutine reconnects with exponential backoff (1s → 2s → 4s, capped at 30s). While reconnecting the snapshot on that pod may be stale. The pod continues serving requests from its last-known snapshot. Monitor `INFO` logs for `cluster: NOTIFY watcher: reconnected successfully` and `WARN` logs for reconnect failures.

**Writer pod also reloads:** the pod that performed the write calls `Reload()` synchronously after the data write (existing behavior, unchanged). It does not need to wait for its own NOTIFY to come back.

**No listeners on a single-pod deployment:** the producer emits `pg_notify` unconditionally. With no listeners in PG, the message is discarded at commit. There is no per-write overhead beyond the extra round-trip to Postgres for `SELECT pg_notify(...)`, which is sub-millisecond and off the hot path.

**Notify call itself fails:** treated as a warning, not an error. The data write has already been committed. Losing one NOTIFY means one pod misses one fanout and stays stale until the next notification. If the condition persists, monitor the `WARN storage: pg_notify relay_catalog failed` log line.

## Log lines to monitor

| Level | Message | Meaning |
|-------|---------|---------|
| `INFO` | `cluster mode enabled: subscribed to relay_catalog NOTIFY` | Watcher started successfully at boot |
| `DEBUG` | `cluster: received relay_catalog notification` | Notification received; reload triggered |
| `WARN` | `cluster: catalog reload after NOTIFY failed` | Reload failed after notification; snapshot may be stale |
| `WARN` | `cluster: NOTIFY watcher: connection lost, reconnecting` | LISTEN connection dropped; reconnecting |
| `WARN` | `cluster: NOTIFY watcher: reconnect failed` | Reconnect attempt failed; will retry |
| `INFO` | `cluster: NOTIFY watcher: reconnected successfully` | Reconnect succeeded |
| `WARN` | `storage: pg_notify relay_catalog failed (non-fatal)` | NOTIFY emit failed; data write still committed |
