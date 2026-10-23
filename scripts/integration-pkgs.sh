#!/usr/bin/env bash
# Prints the packages that hold integration-tagged tests, one "<module dir> <import path>" per line.
# With a list of changed files on stdin (and AFFECTED=1), prints only those whose test binary
# depends on a changed package; changes that can reach every test (migrations, module files,
# the Makefile, build.env, CI definitions) select all of them.
#
#   scripts/integration-pkgs.sh                                  # every integration package
#   git diff --name-only A B | AFFECTED=1 scripts/integration-pkgs.sh
set -euo pipefail
cd "$(dirname "$0")/.."

modules=(. jobq auth secret)
root=github.com/wyolet/relay

changed=()
if [ "${AFFECTED:-}" = 1 ]; then
  mapfile -t changed
  for f in "${changed[@]}"; do
    case "$f" in
      migrations/*|*go.mod|*go.sum|go.work*|Makefile|build.env|.github/workflows/ci-dev.yml|scripts/integration-pkgs.sh|scripts/ci-dev.sh)
        AFFECTED=0; break ;;
    esac
  done
fi

# Changed directories as import paths; every module path is the root path plus its directory.
declare -A touched=()
for f in "${changed[@]}"; do
  d=$(dirname "$f")
  [ "$d" = . ] && touched[$root]=1 || touched[$root/$d]=1
done

for m in "${modules[@]}"; do
  # The root module's tree also holds the other modules; they are listed on their own.
  mapfile -t dirs < <(cd "$m" && grep -rl --include='*_test.go' '^//go:build integration' . | sed 's#/[^/]*$##' \
    | { if [ "$m" = . ]; then grep -vE '^\./(jobq|auth|sdk|secret)(/|$)'; else cat; fi; } | sort -u)
  [ "${#dirs[@]}" -gt 0 ] || continue
  # One line per test binary: "<package>.test <deps...>".
  (cd "$m" && go list -tags=integration -test -f '{{.ImportPath}} {{join .Deps " "}}' "${dirs[@]}") \
    | while read -r bin deps; do
        [ "${bin%.test}" != "$bin" ] || continue
        pkg=${bin%.test}
        if [ "${AFFECTED:-}" != 1 ]; then echo "$m $pkg"; continue; fi
        for dep in $pkg $deps; do
          [ -n "${touched[$dep]:-}" ] && { echo "$m $pkg"; break; }
        done
      done
done
