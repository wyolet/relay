#!/usr/bin/env bash
# Pins a relay-catalog release in versions.env: downloads its data tarball,
# verifies the release's signed build attestation and that the tarball matches
# the release's published .sha256 (the sum relays check at boot), and writes
# CATALOG_VERSION and the tarball's sha256. The image build checks against
# that hash, so the release assets are trusted once, here.
set -euo pipefail
cd "$(dirname "$0")/.."

version=${1:?usage: scripts/pin-catalog.sh vX.Y.Z}
command -v gh >/dev/null || { echo "pin-catalog: 'gh' is required to verify the release attestation" >&2; exit 1; }
grep -q '^CATALOG_SHA256=' versions.env || { echo "pin-catalog: versions.env has no CATALOG_SHA256 line" >&2; exit 1; }

tarball="relay-catalog-data-${version}.tar.gz"
base="https://github.com/wyolet/relay-catalog/releases/download/${version}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl -fsSL -o "$work/$tarball" "$base/$tarball" \
  || { echo "pin-catalog: relay-catalog release ${version} has no ${tarball}" >&2; exit 1; }
curl -fsSL -o "$work/$tarball.sha256" "$base/$tarball.sha256" \
  || { echo "pin-catalog: relay-catalog release ${version} has no ${tarball}.sha256" >&2; exit 1; }
gh attestation verify "$work/$tarball" --repo wyolet/relay-catalog >/dev/null \
  || { echo "pin-catalog: ${tarball} has no valid attestation from wyolet/relay-catalog" >&2; exit 1; }
sum=$(shasum -a 256 "$work/$tarball" | cut -d' ' -f1)
[ "$(cut -d' ' -f1 "$work/$tarball.sha256")" = "$sum" ] \
  || { echo "pin-catalog: ${tarball} does not match the release's ${tarball}.sha256" >&2; exit 1; }

sed -e "s/^CATALOG_VERSION=.*/CATALOG_VERSION=${version}/" -e "s/^CATALOG_SHA256=.*/CATALOG_SHA256=${sum}/" versions.env >"$work/versions.env"
cat "$work/versions.env" >versions.env
echo "pin-catalog: relay-catalog ${version} pinned (sha256 ${sum})"
