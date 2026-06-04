#!/usr/bin/env bash
# One command: start the load-test stack against a relay image, run the matrix,
# write results/report-<ts>.md, tear down.
#
#   RELAY_IMAGE=ghcr.io/wyolet/relay:0.1.1 ./run.sh
#
# Env: LT_KEEP=1 leaves the stack up; LT_SOAK=1 runs the soak scenario only.
set -euo pipefail
cd "$(dirname "$0")"
: "${RELAY_IMAGE:?set RELAY_IMAGE to the relay artifact under test}"
export RELAY_MASTER_KEY="${RELAY_MASTER_KEY:-$(openssl rand -base64 32)}"
export RELAY_ADMIN_TOKEN="${RELAY_ADMIN_TOKEN:-loadtest-admin-token}"
export LT_CTRL_PORT="${LT_CTRL_PORT:-8081}" LT_LB_PORT="${LT_LB_PORT:-8080}" LT_PROM_PORT="${LT_PROM_PORT:-9099}"
C="docker compose -f compose.yml"

echo "▸ clearing any prior stack"
$C down -v --remove-orphans >/dev/null 2>&1 || true
echo "▸ building helpers + starting stack (image: $RELAY_IMAGE)"
$C build mock-fast loadgen >/dev/null
$C up -d postgres valkey mock-fast mock-slow mock-stream recorded relay-a relay-b nginx control prometheus

echo "▸ waiting for relay control plane"
for i in $(seq 1 90); do
  curl -fsS "http://localhost:${LT_CTRL_PORT}/version" >/dev/null 2>&1 && break
  sleep 2
  [ "$i" = 90 ] && { echo "relay never came up"; $C logs relay-a | tail -30; exit 1; }
done

echo "▸ seeding routes"
LT_CTRL="http://localhost:${LT_CTRL_PORT}" python3 seed.py

echo "▸ running matrix"
LT_PROM="http://localhost:${LT_PROM_PORT}" python3 harness.py

if [ "${LT_KEEP:-0}" = "1" ]; then
  echo "▸ LT_KEEP=1 — stack left running (LB :$LT_LB_PORT, ctrl :$LT_CTRL_PORT, prom :$LT_PROM_PORT)"
else
  echo "▸ tearing down"; $C down -v >/dev/null 2>&1
fi
