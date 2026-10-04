#!/usr/bin/env bash
# Builds relay with the UI pinned in versions.env embedded, boots it against an
# empty Postgres (RELAY_PG_DSN) with the pinned catalog version, and checks both
# planes answer, the UI is served, and the catalog seeded at that version.
# A versions.env bump naming a missing or broken release fails here instead of
# at release time. Overwrites cmd/relay/web/dist, as `make ui-fetch` does.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${RELAY_PG_DSN:?set RELAY_PG_DSN to an empty Postgres database}"
set -a
# shellcheck source=/dev/null
. ./versions.env
set +a

DATA_PORT=${DATA_PORT:-18080}
CONTROL_PORT=${CONTROL_PORT:-18081}
BOOT_TIMEOUT_S=${BOOT_TIMEOUT_S:-120}

work=$(mktemp -d)
pid=
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "boot-smoke: $*" >&2
  echo "--- relay log ---" >&2
  cat "$work/relay.log" >&2 2>/dev/null || true
  exit 1
}

tarball="relay-ui-${UI_VERSION}.tar.gz"
base="https://github.com/wyolet/relay-ui/releases/download/${UI_VERSION}"
curl -fsSL -o "$work/$tarball" "$base/$tarball" || fail "UI release $UI_VERSION has no $tarball"
if curl -fsSL -o "$work/$tarball.sha256" "$base/$tarball.sha256"; then
  (cd "$work" && shasum -a 256 -c "$tarball.sha256") || fail "UI $tarball checksum mismatch"
fi
dist=cmd/relay/web/dist
find "$dist" -mindepth 1 ! -name .gitkeep -delete
tar -xz -C "$dist" --strip-components=1 -f "$work/$tarball"
[ -f "$dist/index.html" ] || fail "UI $tarball has no index.html at its root"

CGO_ENABLED=0 go build -trimpath -o "$work/relay" ./cmd/relay

admin_token=$(openssl rand -hex 24)
RELAY_PORT=$DATA_PORT \
RELAY_CONTROL_PORT=$CONTROL_PORT \
RELAY_MASTER_KEY=$(openssl rand -base64 32) \
RELAY_ADMIN_TOKEN=$admin_token \
RELAY_ADMIN_PASSWORD=$(openssl rand -hex 16) \
RELAY_CATALOG_VERSION=$CATALOG_VERSION \
RELAY_EVENTLOG_DIR=$work \
  "$work/relay" >"$work/relay.log" 2>&1 &
pid=$!

data="http://127.0.0.1:$DATA_PORT"
control="http://127.0.0.1:$CONTROL_PORT"
auth="Authorization: Bearer $admin_token"

# The marker is written only after the pinned catalog seeded.
seeded=
for _ in $(seq "$BOOT_TIMEOUT_S"); do
  kill -0 "$pid" 2>/dev/null || fail "relay exited during boot"
  seeded=$(curl -fsS -H "$auth" "$control/api/settings/catalog-source" 2>/dev/null | jq -r '.value.version // empty' || true)
  [ "$seeded" = "$CATALOG_VERSION" ] && break
  sleep 1
done
[ "$seeded" = "$CATALOG_VERSION" ] || fail "catalog $CATALOG_VERSION not seeded within ${BOOT_TIMEOUT_S}s (marker: '${seeded}')"

curl -fsS "$data/healthz" >/dev/null || fail "GET /healthz failed"
curl -fsS "$control/openapi.json" | jq -e '.paths | length > 0' >/dev/null || fail "GET /openapi.json has no paths"
curl -fsS "$control/" | grep -qi '<html' || fail "control / does not serve the UI"
models=$(curl -fsS -H "$auth" "$control/api/models" | jq '.items | length') || fail "GET /api/models failed"
[ "$models" -gt 0 ] || fail "catalog $CATALOG_VERSION seeded no models"

echo "boot-smoke: UI $UI_VERSION served, catalog $CATALOG_VERSION seeded ($models models)"
