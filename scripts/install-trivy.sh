#!/usr/bin/env bash
# Installs the pinned trivy release (linux/amd64) into the given directory,
# checked against the release's SHA-256 so a replaced asset fails the install.
set -euo pipefail
TRIVY_VERSION=0.75.0
TRIVY_SHA256=c6e65abddb348e25f10549df887045629cf28cc72453cd1c63acb717316b3f3f
dir=${1:?usage: install-trivy.sh <dir>}
mkdir -p "$dir"
tgz="$dir/trivy.tar.gz"
curl -fsSLo "$tgz" "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz"
echo "${TRIVY_SHA256}  ${tgz}" | sha256sum -c -
tar -xzf "$tgz" -C "$dir" trivy
rm "$tgz"
