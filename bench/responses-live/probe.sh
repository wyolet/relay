#!/usr/bin/env bash
# Cross-shape Responses verification probe — exercises the PR #332-#336 fixes
# against a REAL openai_responses upstream, cheaply.
#
# The client sends OpenAI Chat-Completions (CC inbound) to a model bound to the
# openai_responses adapter, so relay translates CC -> Responses upstream and
# back. That drives the Responses adapter's SerializeRequest / ParseResponse /
# stream paths — exactly the surface the catalog flip lit up (#319-#332).
#
# Token spend is tiny: short prompts, small max_tokens. The decisive gate is
# test [2] (finish_reason=length), the #333 regression.
set -uo pipefail
cd "$(dirname "$0")"
[ -f env.sh ] || { echo "missing env.sh — copy env.sh.example to env.sh and fill it"; exit 2; }
. ./env.sh
command -v jq >/dev/null || { echo "jq is required"; exit 2; }

PASS=0; FAIL=0; WARN=0
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
no()   { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }
warn() { echo "  WARN: $1"; WARN=$((WARN+1)); }
err()  { echo "$1" | jq -c '.error? // .' 2>/dev/null | head -c 220; }

cc() { # POST a chat-completions body, echo the response
  curl -sS -X POST "$RELAY_BASE/openai/v1/chat/completions" \
    -H "Authorization: Bearer $RELAY_KEY" -H "Content-Type: application/json" -d "$1"
}

echo "== Responses cross-shape probe :: model=$MODEL base=$RELAY_BASE =="

# [1] finish_reason = stop (baseline normal completion)
echo "[1] finish_reason=stop on a normal completion"
r=$(cc "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly the word: hello\"}],\"max_tokens\":16,\"temperature\":0}")
fr=$(echo "$r" | jq -r '.choices[0].finish_reason // "ERR"')
[ "$fr" = "stop" ] && ok "finish_reason=stop" || no "expected stop, got '$fr' :: $(err "$r")"

# [2] finish_reason = length on truncation  <-- THE #333 regression (was 'stop')
echo "[2] finish_reason=length on a truncated completion (max_tokens=1)   <-- #333"
r=$(cc "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Write a 300 word essay about the ocean.\"}],\"max_tokens\":1,\"temperature\":0}")
fr=$(echo "$r" | jq -r '.choices[0].finish_reason // "ERR"')
if [ "$fr" = "length" ]; then ok "finish_reason=length (truncation surfaces, not masked as stop)"
else no "expected length, got '$fr' — if this is 'stop' the #333 fix is NOT live :: $(err "$r")"; fi

# [3] streaming: terminal finish_reason + usage + [DONE]
echo "[3] streaming terminal frame"
s=$(curl -sS -N -X POST "$RELAY_BASE/openai/v1/chat/completions" \
  -H "Authorization: Bearer $RELAY_KEY" -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hi.\"}],\"max_tokens\":16,\"temperature\":0,\"stream\":true,\"stream_options\":{\"include_usage\":true}}")
echo "$s" | grep -q '"finish_reason":"' && ok "stream carried a finish_reason" || no "no finish_reason in stream"
echo "$s" | grep -q '"usage"'            && ok "stream carried usage"          || warn "no usage in stream (include_usage)"
echo "$s" | grep -q 'data: \[DONE\]'     && ok "stream terminated with [DONE]"  || no "no [DONE] terminator"

# [4] tool_calls + tool-result round-trip (#324 reasoning/tool pairing).
#     Informational on nano: a small model may decline the tool — that's model
#     behaviour, not a relay bug. Only a hard FAIL if relay errors or the
#     second turn (the round-trip) breaks.
echo "[4] tool_calls round-trip (reasoning/tool pairing)"
tools='[{"type":"function","function":{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]'
q='What is the weather in Paris? Call get_weather.'
r=$(cc "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$q\"}],\"tools\":$tools,\"tool_choice\":\"required\",\"max_tokens\":64,\"temperature\":0}")
fr=$(echo "$r" | jq -r '.choices[0].finish_reason // "ERR"')
cid=$(echo "$r" | jq -r '.choices[0].message.tool_calls[0].id // empty')
if [ "$fr" = "tool_calls" ] && [ -n "$cid" ]; then
  ok "model returned tool_calls (id=$cid)"
  asst=$(echo "$r" | jq -c '.choices[0].message')
  r2=$(cc "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$q\"},$asst,{\"role\":\"tool\",\"tool_call_id\":\"$cid\",\"content\":\"18C and sunny\"}],\"tools\":$tools,\"max_tokens\":32,\"temperature\":0}")
  fr2=$(echo "$r2" | jq -r '.choices[0].finish_reason // "ERR"')
  [ "$fr2" = "stop" ] && ok "tool-result round-trip completed (reasoning sibling travelled with the call)" \
    || no "round-trip 2nd turn failed: finish=$fr2 :: $(err "$r2")"
elif [ -n "$(echo "$r" | jq -r '.error? // empty')" ]; then
  no "relay errored on the tool request :: $(err "$r")"
else
  warn "model did not emit tool_calls (finish=$fr) — nano models are flaky here; rerun with a larger mini. Not a relay bug."
fi

echo
echo "== $PASS passed, $FAIL failed, $WARN warnings =="
echo "   (gate = 0 failures; warnings are model-behaviour, not relay)"
[ "$FAIL" -eq 0 ]
