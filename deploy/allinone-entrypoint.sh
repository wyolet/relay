#!/bin/sh
# Entrypoint for the all-in-one (embedded-Postgres) relay image.
#
# Boots a local Postgres using the official postgres image's own
# docker-entrypoint (handles first-run initdb + role/db creation from the
# POSTGRES_* env), waits for it to accept connections, then starts relay against
# it. Single container, no external services — `docker run` just works.
#
# This is the demo / single-node convenience image. Production runs the lean
# image against a managed Postgres (and scales to multiple pods, which embedded
# PG cannot do). POSIX sh only — the postgres:alpine base ships busybox ash.
set -eu

PGUSER_LOCAL="${POSTGRES_USER:-relay}"

# Start Postgres in the background via its official entrypoint (re-execs as the
# postgres user, runs initdb on an empty PGDATA, applies POSTGRES_* on first
# boot). No pipe — keep $PG_PID pointing at postgres itself.
docker-entrypoint.sh postgres &
PG_PID=$!

echo "[entrypoint] waiting for postgres…"
until pg_isready -h 127.0.0.1 -U "$PGUSER_LOCAL" >/dev/null 2>&1; do
  kill -0 "$PG_PID" 2>/dev/null || { echo "[entrypoint] postgres exited during startup"; exit 1; }
  sleep 0.5
done
echo "[entrypoint] postgres ready — starting relay"

/relay &
RELAY_PID=$!

term() { kill -TERM "$RELAY_PID" "$PG_PID" 2>/dev/null || true; }
trap term TERM INT

# Take the container down as soon as either process dies. Poll instead of
# `wait -n` (not in busybox ash).
while kill -0 "$RELAY_PID" 2>/dev/null && kill -0 "$PG_PID" 2>/dev/null; do
  sleep 1
done
echo "[entrypoint] a process exited — shutting down"
term
wait || true
