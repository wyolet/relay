#!/usr/bin/env bash
# Snapshot the observability flow after a probe run: Prometheus request metrics
# + the per-request /logs records + a finish_reason summary. Confirms that a
# request both incremented the metrics AND produced a log row end-to-end.
set -uo pipefail
cd "$(dirname "$0")"
[ -f env.sh ] || { echo "missing env.sh — copy env.sh.example to env.sh and fill it"; exit 2; }
. ./env.sh
command -v jq >/dev/null || { echo "jq is required"; exit 2; }
AUTH=(-H "Authorization: Bearer $ADMIN_TOKEN")

echo "== Prometheus (relay_*) :: $CTRL_BASE/metrics =="
# /metrics is typically unauthenticated; fall back to the admin token if not.
m=$(curl -sS "$CTRL_BASE/metrics" 2>/dev/null)
echo "$m" | grep -q '^relay_' || m=$(curl -sS "${AUTH[@]}" "$CTRL_BASE/metrics" 2>/dev/null)
echo "$m" | grep -E '^relay_(requests_total|request_seconds_count|overhead_seconds_(sum|count)|post_flight_seconds_(sum|count)|inflight_requests|admission_seconds_count)' | sort | head -40
echo "  ^ relay_overhead_seconds = THE wedge metric (relay's own time, total minus upstream)."
echo "    relay_requests_total{source,status} = traffic by runner and 2xx/4xx/5xx class."
echo

echo "== recent /logs (last 10) :: finish_reason / status / tokens =="
curl -sS "${AUTH[@]}" "$CTRL_BASE/logs?limit=10" \
  | jq -r '.logs[]? | "\(.ts)  src=\(.source)  http=\(.status)  finish=\(.finish_reason // "-")  model=\(.model_id // .requested_model // "-")  in=\(.tokens.input // 0) out=\(.tokens.output // 0)"' \
  || echo "  (no logs or shape changed — check $CTRL_BASE/logs manually)"
echo

echo "== /usage/summary grouped by finish_reason (did 'length' actually land?) =="
curl -sS "${AUTH[@]}" "$CTRL_BASE/usage/summary?group_by=finish_reason" | jq . 2>/dev/null | head -60
