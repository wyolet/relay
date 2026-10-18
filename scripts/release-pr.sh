#!/usr/bin/env bash
# Fast-forwards release/next to a dev commit whose full CI run passed, and keeps
# one open pull request release/next → main whose description is the draft
# release notes since the latest release plus the coordination flags found in
# that range. Never merges and never tags.
#
#   scripts/release-pr.sh <sha> <run-url>
#   DRY_RUN=1 scripts/release-pr.sh <sha> <run-url>   # print the description only
#   SINCE=v0.10.0 ...                                  # compare from another tag
#
# Reads the range from the local clone, which needs the release tags and <sha>
# (a full-history checkout), not from the compare API, which lists at most 300
# files. Publishing needs gh with contents + pull-requests write on
# GITHUB_REPOSITORY.
set -euo pipefail

sha=${1:?usage: release-pr.sh <sha> <run-url>}
run_url=${2:?usage: release-pr.sh <sha> <run-url>}
repo=${GITHUB_REPOSITORY:-wyolet/relay}
branch=release/next
api="repos/$repo"

last=${SINCE:-$(gh api "$api/releases/latest" --jq .tag_name)}
# Squash commits only; merges of main back into dev carry nothing new.
commits=$(git log --no-merges --format='%h %s' --abbrev=7 "$last..$sha")
files=$(git diff --name-status "$(git merge-base "$last" "$sha")" "$sha")
total=$(git rev-list --count "$last..$sha")

section() { # title, subject pattern
  local lines
  lines=$(grep -E "^[0-9a-f]{7} $2" <<<"$commits" || true)
  [ -n "$lines" ] || return 0
  printf '### %s\n\n' "$1"
  while read -r short subject; do printf -- '- %s (%s)\n' "$subject" "$short"; done <<<"$lines"
  echo
}

flags() {
  local hits
  hits=$(grep -E '^[A-Z][0-9]*\s+versions\.env$' <<<"$files" || true)
  [ -z "$hits" ] || echo "- [ ] \`versions.env\` changed: the bundled UI or catalog moves; check those releases are out."
  hits=$(grep -E '^A\s+(.*/)?migrations/.*\.up\.sql$' <<<"$files" | cut -f2 || true)
  if [ -n "$hits" ]; then
    echo "- [ ] New migrations; rehearse the upgrade on a copy of production data:"
    while read -r m; do echo "  - \`$m\`"; done <<<"$hits"
  fi
  hits=$(grep -E '^[A-Z][0-9]*\s+app/(httpapi/control|catalogview)/' <<<"$files" | grep -v '_test\.go$' | cut -f2 || true)
  [ -z "$hits" ] || echo "- [ ] Control API code changed ($(wc -l <<<"$hits" | tr -d ' ') files under \`app/httpapi/control/\`, \`app/catalogview/\`): if its shape changed, the UI needs a release to match."
}

conventional='(fix|feat|chore|docs|ci|build|test|refactor|perf)(\([^)]*\))?!?:'
body=$(
  echo "Draft release notes: $total commits on dev since $last, up to \`${sha:0:12}\`."
  echo
  echo "Full CI passed on this commit: $run_url"
  echo
  section Fixes 'fix(\(|!|:)'
  section Features 'feat(\(|!|:)'
  section Chores '(chore|ci|build|test|refactor|perf)(\(|!|:)'
  section Docs 'docs(\(|!|:)'
  other=$(grep -vE "^[0-9a-f]{7} $conventional" <<<"$commits" || true)
  if [ -n "$other" ]; then
    printf '### Other\n\n'
    while read -r short subject; do printf -- '- %s (%s)\n' "$subject" "$short"; done <<<"$other"
    echo
  fi
  echo "### Coordination"
  echo
  f=$(flags)
  echo "${f:-None detected.}"
  echo
  echo "Merge with a merge commit, never a squash. The merge is a new commit on main: wait for its push CI to go green, then \`make release\`."
)

if [ "${DRY_RUN:-}" = 1 ]; then
  printf '%s\n' "$body"
  exit 0
fi

# Fast-forward only: the PATCH fails rather than rewrite release/next.
if gh api "$api/git/ref/heads/$branch" >/dev/null 2>&1; then
  gh api -X PATCH "$api/git/refs/heads/$branch" -f sha="$sha" -F force=false >/dev/null \
    || { echo "::error::$branch cannot fast-forward to $sha; refusing to rewrite it" >&2; exit 1; }
else
  gh api -X POST "$api/git/refs" -f ref="refs/heads/$branch" -f sha="$sha" >/dev/null
fi

title="Release: dev at ${sha:0:7}"
pr=$(gh pr list --repo "$repo" --head "$branch" --base main --state open --json number --jq '.[0].number // empty')
if [ -n "$pr" ]; then
  gh pr edit "$pr" --repo "$repo" --title "$title" --body "$body" >/dev/null
else
  gh pr create --repo "$repo" --head "$branch" --base main --title "$title" --body "$body" >/dev/null
fi
echo "$branch -> $sha"
