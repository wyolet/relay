#!/usr/bin/env bash
# Builds relay with the UI pinned in versions.env embedded, checks the pinned
# catalog tarball against its hash, boots relay against an empty Postgres
# (RELAY_PG_DSN) with the pinned catalog version, and checks both planes
# answer, the UI is served, and the catalog seeded at that version.
# A versions.env bump naming a missing or broken release fails here instead of
# at release time. Overwrites cmd/relay/web/dist, as `make ui-fetch` does.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${RELAY_PG_DSN:?set RELAY_PG_DSN to an empty Postgres database}"
set -a
# shellcheck source=/dev/null
. ./versions.env
# shellcheck source=/dev/null
. ./build.env
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
[ -n "${UI_SHA256:-}" ] || fail "versions.env has no UI_SHA256; pin it with 'make ui-pin UI_VERSION=$UI_VERSION'"
curl -fsSL -o "$work/$tarball" "$base/$tarball" || fail "UI release $UI_VERSION has no $tarball"
(cd "$work" && echo "$UI_SHA256  $tarball" | shasum -a 256 -c -) || fail "UI $tarball does not match UI_SHA256 in versions.env"
dist=cmd/relay/web/dist
find "$dist" -mindepth 1 ! -name .gitkeep -delete
tar -xz -C "$dist" --strip-components=1 -f "$work/$tarball"
[ -f "$dist/index.html" ] || fail "UI $tarball has no index.html at its root"

# The image bakes this tarball; relay below fetches the release from its default URL.
catalog="relay-catalog-data-${CATALOG_VERSION}.tar.gz"
[ -n "${CATALOG_SHA256:-}" ] || fail "versions.env has no CATALOG_SHA256; pin it with 'make catalog-pin CATALOG_VERSION=$CATALOG_VERSION'"
curl -fsSL -o "$work/$catalog" "https://github.com/wyolet/relay-catalog/releases/download/${CATALOG_VERSION}/$catalog" \
  || fail "catalog release $CATALOG_VERSION has no $catalog"
(cd "$work" && echo "$CATALOG_SHA256  $catalog" | shasum -a 256 -c -) || fail "catalog $catalog does not match CATALOG_SHA256 in versions.env"

CGO_ENABLED=0 go build -trimpath -o "$work/relay" ./cmd/relay

admin_token=$(openssl rand -hex 24)
RELAY_PORT=$DATA_PORT \
RELAY_CONTROL_PORT=$CONTROL_PORT \
RELAY_MASTER_KEY=$(openssl rand -base64 32) \
RELAY_ADMIN_TOKEN=$admin_token \
RELAY_ADMIN_PASSWORD=$(openssl rand -hex 16) \
RELAY_CATALOG_VERSION=$CATALOG_VERSION \
RELAY_EVENTLOG_DIR=$work \
RELAY_BATCH_PAYLOAD_DIR=$work/batch \
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

# /healthz turns ready only once the seeded catalog's snapshot is installed, a moment after the marker.
ready=
for _ in $(seq "$BOOT_TIMEOUT_S"); do
  curl -fsS "$data/healthz" >/dev/null 2>&1 && ready=1 && break
  sleep 1
done
[ -n "$ready" ] || fail "GET /healthz not ready within ${BOOT_TIMEOUT_S}s"
curl -fsS "$control/api/openapi.json" | jq -e '.paths | length > 0' >/dev/null || fail "GET /api/openapi.json has no paths"
curl -fsS "$control/" | grep -qi '<html' || fail "control / does not serve the UI"
models=$(curl -fsS -H "$auth" "$control/api/models" | jq '.total') || fail "GET /api/models failed"
[ "$models" -gt 0 ] || fail "catalog $CATALOG_VERSION seeded no models"

echo "boot-smoke: UI $UI_VERSION served, catalog $CATALOG_VERSION seeded ($models models)"
