#!/usr/bin/env bash
# Reviews one unit and writes {"unit","kind","ok","cost","result"} to <out>. The result follows
# scripts/review-schema.json. This is the only place that knows which agent does the review; the
# agent gets the checkout read-only and no GitHub token.
#
#   scripts/review-run.sh <context-dir> <kind> <unit-id> <out>
#     kind: area | approach | cross
#   <context-dir> holds units.json (scripts/review-units.sh) and prs.json ([{pr,title,body}]);
#   a cross unit also reads the area results in <context-dir>/results/*.json.
set -uo pipefail
cd "$(dirname "$0")/.." || exit

ctx=${1:?usage}; kind=${2:?usage}; id=${3:?usage}; out=${4:?usage}
model=${REVIEW_MODEL:?set REVIEW_MODEL}
max_diff=${REVIEW_MAX_DIFF_BYTES:-150000}
units=$ctx/units.json
from=$(jq -r .from "$units"); to=$(jq -r .to "$units")
prompt=$(mktemp)

pr_context() { # pr numbers... → titles and descriptions
  for n in "$@"; do
    jq -r --argjson n "$n" '.[] | select(.pr == $n) | "### #\(.pr) \(.title)\n\n\(.body)\n"' "$ctx/prs.json"
  done
}

rules='Read CLAUDE.md first: its conventions and codebase rules (the 11 canonical-protocol rules, the hot-path rules, the kv and storage conventions) are what you check against. For files under .github/, also read .claude/ci.md.'

report_scope='Report only: bugs, codebase-rule violations, security problems, missing tests for new behaviour, concurrency problems and resource leaks. Not style or naming already covered by golangci-lint, and not "consider refactoring". Every finding names the file and the line range at the reviewed commit, what is wrong, a concrete failure scenario (inputs or state and the wrong outcome), the PR it came from, severity and confidence. Set needs_decision when the right fix is a design call. No finding is a valid answer.'

case "$kind" in
  area)
    unit=$(jq -c --arg id "$id" '.areas[] | select(.id == $id)' "$units")
    [ -n "$unit" ] || { echo "no area unit $id" >&2; exit 2; }
    mapfile -t files < <(jq -r '.files[]' <<<"$unit")
    mapfile -t prs < <(jq -r '.prs[]' <<<"$unit")
    {
      echo "You review the code that changed on the dev branch between $from and $to, in the area \`$(jq -r .area <<<"$unit")\`. The checkout is at $to; you can read anything in it and use git, but change nothing."
      echo; echo "$rules Then read the package documentation of the area."
      echo; echo "Changed files. Read each in full, since bugs often sit next to the changed lines:"
      printf -- '- %s\n' "${files[@]}"
      echo; echo "The pull requests that changed them, for intent and trade-offs:"; echo
      pr_context "${prs[@]}"
      echo "The combined diff of the area over the range:"; echo '```diff'
      git diff "$from" "$to" -- "${files[@]}" | head -c "$max_diff"; echo '```'
      echo; echo "$report_scope"
      echo; echo "Also write a 2-4 sentence summary of what changed in the area and why, for a later pass over interactions between areas."
    } >"$prompt"
    ;;
  approach)
    n=$(jq -r --arg id "$id" '.prs[] | select(.id == $id) | .pr' "$units")
    [ -n "$n" ] || { echo "no PR unit $id" >&2; exit 2; }
    sha=$(git log --no-merges --format='%H %s' "$from..$to" | awk -v t="(#$n)" '$NF == t {print $1; exit}')
    {
      echo "You review the approach of one pull request merged into dev, #$n. The checkout is at $to; you can read anything in it and use git, but change nothing."
      echo; echo "$rules"
      echo; pr_context "$n"
      echo "Its change (commit $sha):"; echo '```diff'
      git show --format= "$sha" | head -c "$max_diff"; echo '```'
      echo; echo "Read the surrounding code, then answer: what problem does the PR solve, in one sentence; is the approach correct and the simplest one that fits the codebase's patterns and rules; which two or three other approaches exist and why they fit worse or better. The verdict is fine unless an alternative wins on a concrete criterion you name: correctness, the hot-path or codebase rules, fewer moving parts, consistency with an existing pattern, or reversibility. Taste is not a criterion. Use needs-decision when the choice is a design call."
      echo; echo "Put the answer in approach. Put real defects you find on the way in findings, under the same rules: $report_scope"
      echo; echo "summary: one sentence on what the PR does."
    } >"$prompt"
    ;;
  cross)
    {
      echo "You review interactions between the areas that changed on the dev branch between $from and $to. The checkout is at $to; you can read anything in it and use git, but change nothing."
      echo; echo "$rules"
      echo; echo "Look only for problems between areas: changed contracts and their callers, migrations touching the same table, renamed fields still read elsewhere, configuration added in one place and not wired in another. Each area was reviewed on its own already; don't repeat those findings. Use kind interaction."
      echo; echo "The pull requests in the range:"; echo
      jq -r '.[] | "- #\(.pr) \(.title)"' "$ctx/prs.json"
      echo; echo "What changed in each area:"; echo
      for f in "$ctx"/results/*.json; do
        jq -r 'select(.kind == "area" and .ok) | "- \(.unit): \(.result.summary)"' "$f"
      done
      echo; echo "$report_scope"
      echo; echo "summary: one sentence on the range as a whole."
    } >"$prompt"
    ;;
  *) echo "unknown kind $kind" >&2; exit 2 ;;
esac

raw=$(mktemp)
env -u GH_TOKEN -u GITHUB_TOKEN timeout --signal=TERM "${REVIEW_TIMEOUT:-20m}" claude -p "$(cat "$prompt")" \
  --bare --setting-sources user \
  --settings "{\"availableModels\":[\"$model\"],\"enforceAvailableModels\":true}" \
  --model "$model" \
  --tools "Bash,Read" \
  --allowedTools Read \
    "Bash(git log *)" "Bash(git show *)" "Bash(git diff *)" "Bash(git blame *)" "Bash(git grep *)" \
    "Bash(grep *)" "Bash(rg *)" "Bash(find *)" "Bash(ls *)" "Bash(cat *)" "Bash(head *)" "Bash(tail *)" \
    "Bash(wc *)" "Bash(go doc *)" "Bash(go list *)" \
  --disallowedTools Agent WebFetch WebSearch "mcp__*" \
  --strict-mcp-config --permission-mode dontAsk --permission-prompts none \
  --max-turns "${REVIEW_MAX_TURNS:-60}" --max-budget-usd "${REVIEW_MAX_BUDGET_USD:-5}" \
  --json-schema "$(cat scripts/review-schema.json)" \
  --output-format json --no-session-persistence >"$raw"

# A unit counts only when the agent finished and its answer matches the schema's required shape.
jq -c --arg unit "$id" --arg kind "$kind" '
  {unit: $unit, kind: $kind, cost: (.total_cost_usd // 0), result: .structured_output}
  | .ok = ((.result | type) == "object" and (.result.summary | type) == "string" and (.result.findings | type) == "array")
' "$raw" >"$out" 2>/dev/null || jq -nc --arg unit "$id" --arg kind "$kind" '{unit: $unit, kind: $kind, ok: false, cost: 0, result: null}' >"$out"
jq -c '{unit, kind, ok, cost, findings: (.result.findings // [] | length)}' "$out"
