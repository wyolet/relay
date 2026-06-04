# relay load-test harness

One command spins up a real distributed relay topology against a given image,
runs a scenario matrix at a constant arrival rate, and writes a report.

```
RELAY_IMAGE=ghcr.io/wyolet/relay:0.1.1 ./run.sh
```

→ `results/report-<ts>.md` (+ `.json`), then the stack is torn down.

## What it stands up (`compose.yml`)

```
loadgen ──▶ nginx LB ──▶ relay-a ┐
(vegeta,  (data plane)   relay-b ┴─▶ mock-fast | mock-slow | mock-stream
 off-box)                  │
                        valkey (real Redis Lua) + postgres
                        prometheus (scrapes both pods' /metrics)
```

The relay image is **not** built here — you pass the artifact under test
(`RELAY_IMAGE`). Everything else is built locally (mock + loadgen are tiny Go
binaries in their own modules, so vegeta never touches the relay binary).

## The pieces

| file | role |
|---|---|
| `run.sh` | orchestrator: up → wait → `seed.py` → `harness.py` → down |
| `compose.yml` | the stack, parameterised by `RELAY_IMAGE` |
| `mock/` | one configurable upstream: `fast` \| `slow:Ns` \| `stream` (SSE) |
| `loadgen/` | vegeta-lib generator (constant arrival rate, CO-correct), own module |
| `seed.py` | idempotently wires the 3 mock routes + a relay key into relay |
| `matrix.json` | the scenario list (edit to change rates/durations/mix) |
| `harness.py` | runs each scenario, samples Prometheus, emits the report |

## Why these choices

- **Constant arrival rate** (not closed-loop) → coordinated-omission-correct p99.
- **Generator off the relay hosts** → it never steals relay CPU (the thing that
  capped earlier hand runs).
- **Three mocks** → `fast` isolates overhead, `slow` forces concurrency
  (saturation), `stream` exercises the tee + per-chunk SSE path.
- **Server-side truth from Prometheus** → relay's own `relay_overhead_seconds`,
  CPU, RSS, goroutines across both pods, not just client guesses.

## Knobs

- `LT_KEEP=1` — leave the stack up (poke LB `:8080`, control `:8081`, prom `:9099`).
- `LT_SOAK=1` — run only the long soak scenario (leak / GC-drift hunt).
- `LT_LB_PORT` / `LT_CTRL_PORT` / `LT_PROM_PORT` — relocate published ports.
- Edit `matrix.json` to change the scenario set.

## Roadmap (build around it later)

- Recorded real-traffic sessions via spec-mock-openai as an upstream profile.
- Failure injection (upstream 429/5xx/timeout, key rotation, pod kill).
- Run loadgen as a kube Job for a fully off-cluster generator.
- CI gate: fail the run when p99 / overhead regresses past a threshold.
