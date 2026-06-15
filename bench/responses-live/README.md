# Responses cross-shape live verification kit

Cheap, decisive check that the Responses translation fixes (PRs #332–#336) work
against a **real** `openai_responses` upstream. Run it on the real deployment
with a mini/nano model so the token burn is trivial.

## Why this exists

The fixes were unit-green but never saw a real request. The last time the
catalog flip lit up this path it surfaced four 400s (#319/#321/#322/#323)
*despite* passing tests. This kit closes that gap with ~a handful of tiny
requests.

## What it drives

Client sends **OpenAI Chat-Completions** (`/openai/v1/chat/completions`) for a
model bound to the **`openai_responses`** adapter. Relay translates
CC → Responses-upstream → back to CC, so each request exercises the Responses
adapter's `SerializeRequest` / `ParseResponse` / streaming paths.

| Probe | Fix exercised |
|---|---|
| [1] finish_reason=stop | baseline |
| [2] **finish_reason=length** on `max_tokens:1` | **#333** — the headline (was masked as `stop`) |
| [3] streaming terminal + usage + `[DONE]` | #322/#333 streaming terminal |
| [4] tool_calls + tool-result round-trip | #324 reasoning/tool pairing |

Not covered by the cheap probe (need a hosted-tool or non-native Responses host):
- **#334** hosted-tool item passthrough — only fires if a model auto-invokes a
  built-in tool (e.g. enable `web_search`); covered by unit tests otherwise.
- **#335** hosted-tool `tool_choice` — only reachable on a **Responses-inbound**
  request to a **non-`openai`-named** host (e.g. Azure); a native openai host
  byte-passes and never runs the translator. Add a `/openai/v1/responses` probe
  there if you have such a host.

## Run

```sh
cp env.sh.example env.sh   # then edit: RELAY_BASE, RELAY_KEY, CTRL_BASE, ADMIN_TOKEN, MODEL
./probe.sh                 # exits non-zero if any hard check fails
./metrics.sh               # snapshot Prometheus + /logs + /usage after the probe
```

Green = `probe.sh` reports `0 failed` (warnings are model behaviour, not relay),
and `metrics.sh` shows `relay_requests_total` incremented + `/logs` rows with the
expected `finish_reason` + a `length` bucket in `/usage/summary`.

`env.sh` is gitignored — it holds the key + token.
