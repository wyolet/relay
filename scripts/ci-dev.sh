#!/usr/bin/env bash
# The light CI tier in one job: runs the named parts in parallel, each into its own log, then
# prints every log in its own group and a summary, and fails when any part failed.
#
#   scripts/ci-dev.sh lint test [chart] [integration] [clickhouse]
#
# integration runs the packages scripts/integration-pkgs.sh selects; with CHANGED_FROM set to a
# commit, only those affected by the diff from it. It needs RELAY_TEST_PG_DSN and
# RELAY_TEST_REDIS_ADDR, clickhouse needs RELAY_TEST_CH_DSN.
# The part_* functions are called by name.
# shellcheck disable=SC2329
set -uo pipefail
cd "$(dirname "$0")/.." || exit

logs=$(mktemp -d)
# Test runs never reach a deployment's databases (as GO_TEST in the Makefile).
gotest() { env -u RELAY_PG_DSN -u RELAY_CH_DSN -u RELAY_REDIS_ADDR -u RELAY_OTLP_ENDPOINT go test "$@"; }

part_lint() {
  local unformatted
  unformatted=$(git ls-files -z '*.go' | xargs -0 gofmt -l)
  [ -z "$unformatted" ] || { echo "These files need gofmt:"; echo "$unformatted"; return 1; }
  make --no-print-directory lint-rules && make --no-print-directory lint || return 1
  for m in . sdk jobq auth secret; do (cd "$m" && go build ./...) || return 1; done
}

part_test() { make --no-print-directory test; }

part_chart() { make --no-print-directory chart-lint; }

# No coverage and only the integration-tagged packages: the unit tests run in part_test.
part_integration() {
  local list
  if [ -n "${CHANGED_FROM:-}" ]; then
    list=$(git diff --name-only "$CHANGED_FROM" HEAD | AFFECTED=1 scripts/integration-pkgs.sh) || return 1
  else
    list=$(scripts/integration-pkgs.sh) || return 1
  fi
  [ -n "$list" ] || { echo "No integration package depends on the changed files."; return 0; }
  echo "Integration packages:"; awk '{print "  " $2}' <<<"$list"
  local status=0 m pkgs
  for m in . jobq auth secret; do
    mapfile -t pkgs < <(awk -v m="$m" '$1 == m {print $2}' <<<"$list")
    [ "${#pkgs[@]}" -gt 0 ] || continue
    (cd "$m" && gotest -tags=integration -race -count=1 "${pkgs[@]}") || status=1
  done
  return "$status"
}

part_clickhouse() { make --no-print-directory test-clickhouse-run; }

for p in "$@"; do
  declare -F "part_$p" >/dev/null || { echo "unknown part: $p" >&2; exit 2; }
done

for p in "$@"; do
  (
    start=$SECONDS
    "part_$p" >"$logs/$p.log" 2>&1
    echo "$? $((SECONDS - start))" >"$logs/$p.status"
  ) &
done
wait

failed=0
summary="| part | result | time |"$'\n'"|---|---|---|"
for p in "$@"; do
  read -r code secs <"$logs/$p.status"
  result=passed; [ "$code" = 0 ] || { result=failed; failed=1; }
  echo "::group::$p: $result (${secs}s)"
  cat "$logs/$p.log"
  echo "::endgroup::"
  summary+=$'\n'"| $p | $result | ${secs}s |"
done
# Failing parts again, outside any group, so the error is what the log ends on.
for p in "$@"; do
  read -r code _ <"$logs/$p.status"
  [ "$code" = 0 ] && continue
  echo "::error title=$p failed::see the $p group above"
  tail -n 60 "$logs/$p.log"
done
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf '%s\n' "$summary" >>"$GITHUB_STEP_SUMMARY"
printf '%s\n' "$summary"
exit $failed
