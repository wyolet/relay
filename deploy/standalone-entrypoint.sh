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

# --- auth bootstrap ---
# The standalone image must be enterable on first boot. We seed two parallel
# credentials, both persisted on the PG volume and surviving restarts:
#   1. an admin username/password (a kind:User YAML the /auth/login session flow
#      verifies) — the primary human login;
#   2. an admin bearer token (RELAY_ADMIN_TOKEN) — break-glass control-plane auth
#      for CI/scripts, orthogonal to the user model so it survives a future
#      multi-user identity rewrite.
# Plus a master key for stored-secret encryption. Operator-supplied env / a
# mounted RELAY_CONFIG_DIR always win; we never overwrite either.
BOOTSTRAP_ENV="$PGDATA/relay-bootstrap.env"
RELAY_CONFIG_DIR="${RELAY_CONFIG_DIR:-$PGDATA/relay-config}"
export RELAY_CONFIG_DIR
ADMIN_YAML="$RELAY_CONFIG_DIR/admin-user.yaml"

if [ -f "$BOOTSTRAP_ENV" ]; then
  saved_mk=${RELAY_MASTER_KEY:-}
  saved_tk=${RELAY_ADMIN_TOKEN:-}
  # shellcheck disable=SC1090
  . "$BOOTSTRAP_ENV"
  [ -n "$saved_mk" ] && RELAY_MASTER_KEY=$saved_mk
  [ -n "$saved_tk" ] && RELAY_ADMIN_TOKEN=$saved_tk
fi

new_secret=
if [ -z "${RELAY_MASTER_KEY:-}" ]; then
  # 32 random bytes, base64-std — the exact format crypto.ParseMasterKey expects.
  RELAY_MASTER_KEY=$(head -c 32 /dev/urandom | base64 | tr -d '\n')
  new_secret=1
fi
if [ -z "${RELAY_ADMIN_TOKEN:-}" ]; then
  # sk-wr- prefix matches the house token style (app/relaykey); base64url, unpadded.
  RELAY_ADMIN_TOKEN=sk-wr-$(head -c 48 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')
  new_secret=1
fi
if [ -n "$new_secret" ]; then
  umask 077
  { echo "RELAY_MASTER_KEY='$RELAY_MASTER_KEY'"
    echo "RELAY_ADMIN_TOKEN='$RELAY_ADMIN_TOKEN'"
  } > "$BOOTSTRAP_ENV"
  chown postgres:postgres "$BOOTSTRAP_ENV" 2>/dev/null || true
fi
export RELAY_MASTER_KEY RELAY_ADMIN_TOKEN

# Seed the admin username/password user. Only when the config dir holds no users
# yet — a restart (YAML already there) or an operator-managed config dir is left
# untouched. Plaintext password is accepted by identity.Verify (one deprecation
# log per login); fine for a single-node demo where it sits on the same volume.
admin_pw=
if [ ! -e "$ADMIN_YAML" ] && [ -z "$(ls -A "$RELAY_CONFIG_DIR" 2>/dev/null || true)" ]; then
  admin_pw=$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c1-20)
  mkdir -p "$RELAY_CONFIG_DIR"
  umask 077
  cat > "$ADMIN_YAML" <<EOF
apiVersion: relay.wyolet.dev/v1
kind: User
metadata:
  name: admin
spec:
  username: admin
  email: admin@localhost
  password: '$admin_pw'
  roles:
    - admin
EOF
  chown -R postgres:postgres "$RELAY_CONFIG_DIR" 2>/dev/null || true
fi

if [ -n "$new_secret" ] || [ -n "$admin_pw" ]; then
  { echo
    echo "  relay first-boot — generated admin credentials (saved to the data volume):"
    echo
    echo "    control plane:  http://localhost:8081"
    [ -n "$admin_pw" ] && echo "    username:       admin"
    [ -n "$admin_pw" ] && echo "    password:       $admin_pw"
    echo "    admin token:    $RELAY_ADMIN_TOKEN"
    echo
    echo "  Sign in to the UI with admin / the password above, or use the token:"
    echo "    Authorization: Bearer $RELAY_ADMIN_TOKEN"
    echo "  Override by passing RELAY_ADMIN_TOKEN / RELAY_MASTER_KEY, or mount your"
    echo "  own RELAY_CONFIG_DIR with user YAMLs, at run time."
    echo
  } >&2
fi

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
