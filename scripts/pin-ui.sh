#!/usr/bin/env bash
# Pins a relay-ui release in versions.env: downloads its tarball, verifies the
# release's signed build attestation, and writes UI_VERSION and the tarball's
# sha256. Every later fetch (image build, ui-fetch, boot smoke) checks against
# that hash, so the release assets are trusted once, here.
set -euo pipefail
cd "$(dirname "$0")/.."

version=${1:?usage: scripts/pin-ui.sh vX.Y.Z}
command -v gh >/dev/null || { echo "pin-ui: 'gh' is required to verify the release attestation" >&2; exit 1; }
grep -q '^UI_SHA256=' versions.env || { echo "pin-ui: versions.env has no UI_SHA256 line" >&2; exit 1; }

tarball="relay-ui-${version}.tar.gz"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl -fsSL -o "$work/$tarball" "https://github.com/wyolet/relay-ui/releases/download/${version}/${tarball}" \
  || { echo "pin-ui: relay-ui release ${version} has no ${tarball}" >&2; exit 1; }
gh attestation verify "$work/$tarball" --repo wyolet/relay-ui >/dev/null \
  || { echo "pin-ui: ${tarball} has no valid attestation from wyolet/relay-ui" >&2; exit 1; }
sum=$(shasum -a 256 "$work/$tarball" | cut -d' ' -f1)

sed -e "s/^UI_VERSION=.*/UI_VERSION=${version}/" -e "s/^UI_SHA256=.*/UI_SHA256=${sum}/" versions.env >"$work/versions.env"
cat "$work/versions.env" >versions.env
echo "pin-ui: relay-ui ${version} pinned (sha256 ${sum})"
