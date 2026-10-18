#!/usr/bin/env bash
# Splits the code changed between two commits into review units and prints them as JSON:
#   {"from","to","areas":[{"id","area","files":[...],"prs":[...]}],"prs":[{"id","pr","title"}]}
# An area is a Go package; the auth, sdk, jobq and secret modules are one area each, and
# migrations/, .github/ and chart/ are areas of their own. An area over MAX_AREA_FILES files or
# MAX_AREA_BYTES of current content becomes one unit per file. Docs and generated files are not
# reviewed. Each squash commit names its PR as "(#N)"; every PR that touched reviewed code is also
# a unit of its own.
#
#   scripts/review-units.sh <from> <to>
set -euo pipefail
cd "$(dirname "$0")/.." || exit

from=${1:?usage: review-units.sh <from> <to>}
to=${2:?usage: review-units.sh <from> <to>}
# From the merge base: a release tag sits on main, which may hold commits dev doesn't.
from=$(git merge-base "$from" "$to")
max_files=${MAX_AREA_FILES:-12}
max_bytes=${MAX_AREA_BYTES:-200000}

area_of() {
  case "$1" in
    migrations/*) echo migrations ;;
    .github/*) echo .github ;;
    chart/*) echo chart ;;
    auth/*|sdk/*|jobq/*|secret/*) echo "${1%%/*}" ;;
    *) dirname "$1" ;;
  esac
}

reviewed() {
  case "$1" in
    *.md|*.mdx|docs/*|*go.sum|*/gen/*|*.gz|*.png|*.svg|*/testdata/*) return 1 ;;
  esac
  return 0
}

# Files that still exist at <to>; a deleted file has nothing left to review.
mapfile -t files < <(git diff --name-only --diff-filter=d "$from" "$to")

# PRs per file, from the squash commits that touched it.
declare -A file_prs=()
while read -r sha subject; do
  pr=$(grep -oE '\(#[0-9]+\)$' <<<"$subject" | tr -dc '0-9') || true
  [ -n "$pr" ] || continue
  while read -r f; do
    [ -n "$f" ] && file_prs[$f]+="$pr "
  done < <(git diff-tree --no-commit-id --name-only -r "$sha")
done < <(git log --no-merges --format='%H %s' "$from..$to")

declare -A area_files=() area_bytes=()
for f in "${files[@]}"; do
  reviewed "$f" || continue
  a=$(area_of "$f")
  area_files[$a]+="$f"$'\n'
  area_bytes[$a]=$(( ${area_bytes[$a]:-0} + $(git cat-file -s "$to:$f") ))
done

unit() { # id area files... → one area unit as JSON
  local id=$1 area=$2; shift 2
  local prs
  prs=$(for f in "$@"; do echo "${file_prs[$f]:-}"; done | tr ' ' '\n' | grep -v '^$' | sort -nu | paste -sd, -)
  jq -nc --arg id "$id" --arg area "$area" --argjson prs "[${prs}]" '$ARGS.positional as $f | {id: $id, area: $area, files: $f, prs: $prs}' --args "$@"
}

areas=$(
  for a in $(printf '%s\n' "${!area_files[@]}" | sort); do
    mapfile -t af < <(printf '%s' "${area_files[$a]}")
    slug=$(sed -e 's#^\.$#root#' -e 's#^\.##' -e 's#[/.]#-#g' <<<"$a")
    if [ "${#af[@]}" -gt "$max_files" ] || [ "${area_bytes[$a]}" -gt "$max_bytes" ]; then
      for f in "${af[@]}"; do unit "$slug--$(tr '/.' '--' <<<"${f##*/}")" "$a" "$f"; done
    else
      unit "$slug" "$a" "${af[@]}"
    fi
  done | jq -sc .
)

prs=$(
  jq -r '.[].prs[]' <<<"$areas" | sort -nu \
    | while read -r pr; do
        title=$(git log --no-merges --format='%s' "$from..$to" | grep -m1 -E "\(#$pr\)$" | sed -E 's/ \(#[0-9]+\)$//')
        jq -nc --argjson pr "$pr" --arg title "$title" '{id: ("pr-\($pr)"), pr: $pr, title: $title}'
      done | jq -sc .
)

jq -nc --arg from "$(git rev-parse "$from")" --arg to "$(git rev-parse "$to")" \
  --argjson areas "$areas" --argjson prs "$prs" '{from: $from, to: $to, areas: $areas, prs: $prs}'
