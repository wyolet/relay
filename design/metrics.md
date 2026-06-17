# Metrics

How Relay decides what to measure, and the metrics it ships.

This doc exists because the easy failure mode is to instrument
everything that happens to have a counter, ship 40 metrics, and end up
with a dashboard nobody reads. A metric you don't look at is worse than
no metric — it's noise that hides the handful you actually need. So the
rule here is the opposite of "measure everything."

## The one rule

**A metric earns its place only if it answers a question someone will
actually ask** — at 3am during an incident, or to prove a promise we
made (an SLO). If you can't write down the question in a sentence, don't
add the metric. Add it the day you have the question, not before.

## Name the fear, not the mechanism

The most common way metrics go useless is naming them after *how the
code works internally* instead of *what the operator is worried about*.

> An operator staring at a dashboard during an outage does not think
> "is a circuit breaker open." They think "how many of my provider keys
> are dead right now, and why." The metric name must be the second
> sentence, in the words the worried human is already using.

So: no internal vocabulary in metric names. Not `breaker_open`, not
`reservation_rejected`, not `emitter_backpressure`. Name the thing the
operator is afraid of:

| Internal mechanism | What it's called in code | What the operator fears | Metric name |
|---|---|---|---|
| circuit breaker trips, key benched | `keypool` breaker | "my provider keys are dying" | `relay_provider_keys_down` |
| bounded channel drop-on-full | emitter `Dropped()` | "I'm losing usage records I bill on" | `relay_records_lost_total` |
| handler entry→exit minus upstream | `RequestOverhead` | "is Relay slow, or is the provider slow" | `relay_overhead_seconds` |

If you find yourself explaining a metric name by describing the code,
rename it.

## The two questions every system answers

Two standard lenses, and Relay has two halves that map onto them:

- **Request flow** (the edge that serves customers) — *Rate, Errors,
  Duration*. How many requests, how many failing, how slow.
- **Resources** (pools, queues, anything with finite capacity) —
  *how full, how much is being dropped, are they erroring*.

You don't need to memorize the acronyms (they're RED and USE if you ever
want to look them up). You need to ask, for any new metric: "is this
telling me about request flow or about a resource running out — and do I
already cover that?"

## What Relay actually needs to know

Relay's whole pitch is **a thin, fast layer in front of provider APIs,
with bring-your-own-key pooling.** So the metrics that matter are the
ones that prove or disprove *that specific claim*. Four questions, in
priority order:

### Q1 — Is Relay keeping its speed promise?

This is the wedge. The single most important number in the system is
**Relay's own overhead** — the time Relay adds, *excluding* the upstream
provider call. The performance contract puts a hard SLO on it (p99
< 10 ms live). If we shipped exactly one metric, it would be this one.

### Q2 — When it's slow or broken, is it us or the provider?

The 3am question. Relay's entire value is being thin, so the first thing
you need to know during a latency complaint is **whose time it is.** If
total request time is 8 s but Relay's overhead is 3 ms, the provider is
slow and there is nothing to fix in Relay. This needs the *split*: total
duration alongside overhead. Without it, every slowdown turns into a
witch-hunt through Relay's code when the problem is upstream.

### Q3 — Are we silently losing data?

Relay's background pipelines (usage records, payload capture) drop work
when they back up, on purpose — blocking the request path would be
worse. That's the right design, but a drop you can't *see* is a billing
or audit hole. So we need to watch the drop rate and alert before we
lose records we bill on.

### Q4 — Are the provider keys healthy, or am I one failure from an outage?

Key pooling *is* the product. When keys start failing and getting pulled
from rotation, we're burning through our redundancy and heading toward
"no healthy keys in pool" — a customer-facing outage. We want to see
keys going down *before* requests start failing, as a leading indicator.

### Q5 — Under load, where does Relay saturate first?

The stress-test lens. When you push N
concurrent users at a pod, four things creak before anything else, and
each needs its own leading signal:

- **Open streams** — how many requests are open right now, and where
  does concurrency plateau? The closed-loop saturation signal.
- **Admission** — the pre-upstream leg (auth + rate-limit Lua reserve +
  key selection). This is the Redis-contention proxy: if it spikes,
  *then* drill into kv/Lua with a one-off — without instrumenting
  `pkg/kv` permanently.
- **Post-flight** — is the observer fan-out keeping up with stream
  closes, or building a goroutine backlog?
- **Emit queues** — `records_lost` tells us *after* it's too late; queue
  depth shows the approach to saturation and whether sink draining
  keeps up.

## The shipped set

Nine metrics plus the free runtime baseline. Each ties to a question
above. Namespace is `relay`.

| Question | Metric | Type | Labels | Reads |
|---|---|---|---|---|
| Q1, Q2 | `relay_requests_total` | counter | `source`, `status` | traffic rate + error rate |
| Q1 | `relay_overhead_seconds` | histogram | `source` | **the SLO metric** — Relay's own time |
| Q2 | `relay_request_seconds` | histogram | `source` | total time; vs overhead = the split |
| Q3 | `relay_records_lost_total` | counter | `kind` (usage/payload) | background drops |
| Q4 | `relay_provider_keys_down_total` | counter | `reason` | rate of keys going into cooldown (leading signal) |
| Q5 | `relay_inflight_requests` | gauge | `source` | currently-open requests; streams count until body close |
| Q5 | `relay_admission_seconds` | histogram | `source` | request accept → upstream handoff (Redis-contention proxy) |
| Q5 | `relay_post_flight_seconds` | histogram | — | one full post-flight fan-out (`Registry.Finalize`) |
| Q5 | `relay_emit_queue_depth` | gauge | `kind` (usage/payload) | bounded emitter fill level — drops' leading signal |
| baseline | Go runtime + process | (built-in) | — | goroutines, heap, GC, CPU, FDs |

`source` is the runner the request took (`pipeline`/`proxy`/`ws`/
`batch`) — the lowest-cardinality dimension already on the lifecycle
Context. Wire `shape` (openai/anthropic) is intentionally **not** a
label: it isn't on the Context, and plumbing it would touch the
inference handler. Add it the day "openai vs anthropic split" is a real
question.

`reason` for the key metric is the cooldown class: `upstream_auth_failed`,
`upstream_rate_limited`, `upstream_server_error`, `upstream_network_error`,
`local_rl_exhausted` (the `keypool.CooldownReason` values).

**Why a counter and not a gauge for keys-down:** a "keys down right now"
gauge would be the more natural read, but breaker state lives in shared
kv (Redis), not per-pod memory — a per-pod Prometheus gauge would be
inconsistent across the fleet, and a wrong gauge is worse than none.
Trip *counts* sum cleanly across pods and are the real leading signal. A
faithful global gauge is deferred (needs a kv scan — not lightweight).

## Labels: the rule that keeps this from falling over

Every label value multiplies the number of series stored. `shape` (3) ×
`status` (5) = 15 series for one counter — fine. Add a `model` label
(hundreds of values) and one counter becomes thousands of series; add it
to a histogram (each series is ~12 buckets) and your metrics backend
runs out of memory.

So:

1. **Labels must be low-cardinality and bounded.** A known, small,
   fixed set of values.
2. **`status` is a class** (`2xx`/`4xx`/`5xx`), never the raw code.
3. **No `model` label on histograms.** "What's Relay's p99" is the
   question, not "what's the p99 for one specific model." If per-model
   request *volume* is ever wanted, it goes on the cheap counter only —
   never on a histogram.
4. **Never a per-request id, key value, or anything unbounded as a
   label.**

## What we deliberately did NOT ship

These are real signals with no question behind them *yet*. Each is a
one-metric addition the day someone actually asks:

- per-op kv latency / Lua script timing (`admission_seconds` covers the
  whole pre-upstream leg; drill in with a one-off only if it spikes)
- per-hook lifecycle timing (`post_flight_seconds` covers the fan-out
  total)
- tee-buffer byte gauges (RSS covers it coarsely)
- snapshot reload lag (NOTIFY fan-out time across pods)
- failover and key-refresh (heal) counts
- batch queue depth and job-state counts

Resisting these is the point of this doc. When you add one, add the
*question* to Q5, Q6, … above, so the next person sees why it's there.

## Where it lives

- Metric declarations: `pkg/metrics/` (one file per subsystem; each sets
  `Namespace = "relay"` and a subsystem matching its package).
- Emission: `app/metricslog` is the lifecycle observer — a pre-flight
  middleware (inflight gauge up), a post-flight Hook (the request-flow
  histograms), and a Collector (inflight gauge down) on the shared
  Registry. Key-health metrics are set directly in `pkg/keypool` where
  breakers change state; queue depths are `GaugeFunc`s over the emitter
  channels, registered at boot; the post-flight duration is a callback
  on `lifecycle.Registry` set at boot (the Registry stays metrics-free).
- The `/metrics` endpoint is served on the **control plane** listener
  (ops surface, not customer-facing).
