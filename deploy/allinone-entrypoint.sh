#!/bin/sh
# Entrypoint for the all-in-one (embedded-Postgres) relay image.
#
# Initializes a local Postgres on first run (initdb + create the relay role/db),
# starts it, then starts relay against it on 127.0.0.1. Single container, no
# external services — `docker run` just works.
#
# Built on alpine + the apk postgresql16 package (no LLVM JIT — far smaller than
# the official postgres image). Postgres listens only on loopback and is never
# exposed, so trust auth is fine. Demo / single-node only; production runs the
# lean image against managed PG. POSIX sh (busybox ash).
set -eu

PGBIN=/usr/bin
PGDATA="${PGDATA:-/var/lib/postgresql/data}"
DBUSER="${POSTGRES_USER:-relay}"
DBNAME="${POSTGRES_DB:-relay}"

mkdir -p "$PGDATA" /run/postgresql
chown -R postgres:postgres "$PGDATA" /run/postgresql

if [ ! -s "$PGDATA/PG_VERSION" ]; then
  echo "[entrypoint] first run — initializing postgres…"
  su postgres -c "$PGBIN/initdb -D '$PGDATA' --encoding=UTF8 --auth=trust"
  su postgres -c "$PGBIN/pg_ctl -D '$PGDATA' -o '-c listen_addresses=127.0.0.1' -w start"
  su postgres -c "$PGBIN/psql --no-psqlrc -v ON_ERROR_STOP=1 --dbname postgres \
    --command \"CREATE ROLE \\\"$DBUSER\\\" LOGIN SUPERUSER;\""
  su postgres -c "$PGBIN/createdb -O '$DBUSER' '$DBNAME'"
  su postgres -c "$PGBIN/pg_ctl -D '$PGDATA' -w stop"
fi

echo "[entrypoint] starting postgres…"
su postgres -c "$PGBIN/postgres -D '$PGDATA' -c listen_addresses=127.0.0.1" &
PG_PID=$!

echo "[entrypoint] waiting for postgres…"
until "$PGBIN/pg_isready" -h 127.0.0.1 -U "$DBUSER" >/dev/null 2>&1; do
  kill -0 "$PG_PID" 2>/dev/null || { echo "[entrypoint] postgres exited during startup"; exit 1; }
  sleep 0.5
done
echo "[entrypoint] postgres ready — starting relay"

/relay &
RELAY_PID=$!

term() { kill -TERM "$RELAY_PID" "$PG_PID" 2>/dev/null || true; }
trap term TERM INT

# Take the container down as soon as either process dies (no `wait -n` in ash).
while kill -0 "$RELAY_PID" 2>/dev/null && kill -0 "$PG_PID" 2>/dev/null; do
  sleep 1
done
echo "[entrypoint] a process exited — shutting down"
term
wait || true
