# jobq

`github.com/wyolet/relay/jobq` is a durable background-job queue backed by Postgres. A job is an opaque input payload plus a queue name, and a registered `Handler` turns input bytes into output bytes. jobq handles persistence, claiming across many workers, retries with backoff, crash recovery and concurrency limits. It knows nothing about LLMs or any caller's domain. In relay, it runs the items of the batch API.

```bash
go get github.com/wyolet/relay/jobq@latest
```

How it works:

- **Payloads stay out of Postgres.** A job row holds its state, attempt count, schedule and claim times, and the URIs of its input and result. The bytes themselves live in a `payload.Store`; `payload.FileStore` is the built-in one.
- **Claim and commit.** A worker claims a job by setting it to `running` under `FOR UPDATE SKIP LOCKED` and commits at once. No database connection is held while the handler runs.
- **Crash recovery.** A rescuer, safe to run on every node, reclaims jobs left `running` longer than `RescueAfter`. `RescueAfter` must exceed `JobTimeout`. Delivery is at-least-once, so handlers should be idempotent.
- **Own schema.** `jobq.Migrate` applies the module's migrations and tracks them in its own `jobq_schema_migrations` table.

Not included: leader election, `pg_notify` wake-up (the claim loop polls) and result retention.

Entry points: `jobq.New(pool, store, opts)`, `Queue.Register`, `Queue.Start`, `Queue.Enqueue`, `Queue.Get`, `Queue.Result`, `Queue.Cancel`.

The module is v0: its API may change between minor versions. Releases are tagged `jobq/vX.Y.Z`. It was built for relay, and it imports nothing from the relay server module.

API reference: [pkg.go.dev](https://pkg.go.dev/github.com/wyolet/relay/jobq). License: [Apache-2.0](https://github.com/wyolet/relay/blob/main/LICENSE).
