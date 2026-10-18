#!/usr/bin/env bash
# Turns the review results into GitHub issues and moves the ai-reviewed tag.
#
#   scripts/review-report.sh <context-dir>          # results in <context-dir>/results/*.json
#   DRY_RUN=1 scripts/review-report.sh <context-dir> # print what it would do
#
# A finding of high or medium severity with at least medium confidence gets an issue of its own
# (ai-review + severity:<level>); so does one that needs a design call (needs-decision). An open
# ai-review issue for the same file and kind gets a comment instead of a second issue. Everything
# else, the "fine" approach verdicts and the units that didn't complete go into one digest issue.
# The tag moves to the reviewed commit only when every unit completed.
set -euo pipefail
cd "$(dirname "$0")/.." || exit

ctx=${1:?usage: review-report.sh <context-dir>}
repo=${GITHUB_REPOSITORY:-wyolet/relay}
server=${GITHUB_SERVER_URL:-https://github.com}
run_url=${RUN_URL:-}
from=$(jq -r .from "$ctx/units.json"); to=$(jq -r .to "$ctx/units.json")
range="${from:0:7}..${to:0:7}"

gh_do() { if [ "${DRY_RUN:-}" = 1 ]; then printf 'would run: gh'; printf ' %q' "$@"; echo; else gh "$@"; fi; }

shopt -s nullglob
result_files=("$ctx"/results/*.json)
results='[]'
[ "${#result_files[@]}" -eq 0 ] || results=$(jq -sc . "${result_files[@]}")
expected=$(jq -c '[(.areas[] | {unit: .id, kind: "area"}), (.prs[] | {unit: .id, kind: "approach"})] + [{unit: "cross", kind: "cross"}]' "$ctx/units.json")
skipped=$(jq -nc --argjson e "$expected" --argjson r "$results" '[$e[] | select(. as $u | any($r[]; .unit == $u.unit and .ok) | not) | .unit]')
cost=$(jq -r '[.[].cost] | add // 0 | . * 100 | round / 100' <<<"$results")

# Every finding with the PR it came from; an area finding without one takes the area's only PR.
findings=$(jq -c --slurpfile u "$ctx/units.json" '
  [ .[] | select(.ok) as $r | $r.result.findings[] | . as $f
    | ($u[0].areas[] | select(.id == $r.unit) | .prs) as $prs
    | $f + {unit: $r.unit, pr: ($f.pr // (if ($prs | length) == 1 then $prs[0] else null end))} ]' <<<"$results")
own=$(jq -c '[.[] | select(.needs_decision or ((.severity == "high" or .severity == "medium") and .confidence != "low"))]' <<<"$findings")
minor=$(jq -c '[.[] | select((.needs_decision or ((.severity == "high" or .severity == "medium") and .confidence != "low")) | not)]' <<<"$findings")
approach=$(jq -c --slurpfile u "$ctx/units.json" '[.[] | select(.ok and .kind == "approach" and .result.approach != null) | . as $r
  | ($u[0].prs[] | select(.id == $r.unit)) as $p | $r.result.approach + {pr: $p.pr, title: $p.title}]' <<<"$results")

for l in ai-review:5319e7 approach:c5def5 needs-decision:d93f0b severity:high:b60205 severity:medium:d93f0b severity:low:fbca04; do
  gh_do label create "${l%:*}" --repo "$repo" --color "${l##*:}" --force >/dev/null
done

if [ "${DRY_RUN:-}" = 1 ]; then
  open_issues=${DRY_RUN_OPEN_ISSUES:-[]}
else
  open_issues=$(gh issue list --repo "$repo" --label ai-review --state open --limit 500 --json number,body)
fi
link() { echo "$server/$repo/blob/$to/$1#L${2/-/-L}"; }
opened=(); commented=()

while read -r f; do
  [ -n "$f" ] || continue
  file=$(jq -r .file <<<"$f"); kind=$(jq -r .kind <<<"$f"); lines=$(jq -r .lines <<<"$f")
  key="<!-- ai-review: file=$file kind=$kind -->"
  body=$(jq -r --arg link "$(link "$file" "$lines")" --arg run "$run_url" --arg range "$range" '
    "\(.what)\n\n**Failure scenario:** \(.failure_scenario)\n\n**Where:** \($link)\n\n**From:** \(if .pr then "#\(.pr)" else "the range \($range)" end) · severity \(.severity), confidence \(.confidence)\(if $run != "" then " · [review run](\($run))" else "" end)"' <<<"$f")
  existing=$(jq -r --arg key "$key" '[.[] | select(.body | contains($key)) | .number][0] // empty' <<<"$open_issues")
  if [ -n "$existing" ]; then
    gh_do issue comment "$existing" --repo "$repo" --body "Seen again in $range:"$'\n\n'"$body" >/dev/null
    commented+=("#$existing")
    continue
  fi
  labels="ai-review,severity:$(jq -r .severity <<<"$f")"
  [ "$(jq -r .needs_decision <<<"$f")" = true ] && labels+=",needs-decision"
  title="ai-review: $(jq -r '.what | split(". ")[0] | .[0:90]' <<<"$f") ($file)"
  url=$(gh_do issue create --repo "$repo" --label "$labels" --title "$title" --body "$body"$'\n\n'"$key")
  opened+=("$url")
done < <(jq -c '.[]' <<<"$own")

while read -r a; do
  [ -n "$a" ] || continue
  verdict=$(jq -r .verdict <<<"$a"); [ "$verdict" = fine ] && continue
  labels="ai-review,approach"; [ "$verdict" = needs-decision ] && labels+=",needs-decision"
  body=$(jq -r '"**Problem:** \(.problem)\n\n**Verdict:** \(.verdict). \(.reason)\n\n**Alternatives considered:**\n\(.alternatives | map("- " + .) | join("\n"))"' <<<"$a")
  url=$(gh_do issue create --repo "$repo" --label "$labels" --title "ai-review approach: #$(jq -r .pr <<<"$a") $(jq -r .title <<<"$a")" --body "$body")
  opened+=("$url")
done < <(jq -c '.[]' <<<"$approach")

digest=$(
  echo "Review of \`dev\` $range$( [ -n "$run_url" ] && echo " ([run]($run_url))"). Cost: \$$cost."
  echo
  echo "Issues opened: ${#opened[@]}. Existing issues commented: ${#commented[@]}."
  [ "${#opened[@]}" -eq 0 ] || printf -- '- %s\n' "${opened[@]}"
  [ "${#commented[@]}" -eq 0 ] || printf -- '- %s\n' "${commented[@]}"
  echo
  echo "### Approach"
  jq -r '.[] | "- #\(.pr) \(.title): \(.verdict)"' <<<"$approach"
  echo
  echo "### Minor and low-confidence notes"
  jq -r --arg base "$server/$repo/blob/$to" '.[] | "- [\(.file):\(.lines)](\($base)/\(.file)#L\(.lines | sub("-"; "-L"))) \(.what) (\(.severity), \(.confidence) confidence)"' <<<"$minor"
  [ "$(jq length <<<"$minor")" -gt 0 ] || echo "None."
  echo
  echo "### Not reviewed"
  jq -r '.[] | "- \(.)"' <<<"$skipped"
  [ "$(jq length <<<"$skipped")" -gt 0 ] || echo "Every unit completed."
)
gh_do issue create --repo "$repo" --label ai-review --title "ai-review digest: dev $range" --body "$digest" >/dev/null
[ "${DRY_RUN:-}" = 1 ] && printf '%s\n' "$digest"

if [ "$(jq length <<<"$skipped")" -eq 0 ]; then
  if [ "${DRY_RUN:-}" = 1 ] || gh api "repos/$repo/git/ref/tags/ai-reviewed" >/dev/null 2>&1; then
    gh_do api -X PATCH "repos/$repo/git/refs/tags/ai-reviewed" -f sha="$to" -F force=true >/dev/null
  else
    gh_do api -X POST "repos/$repo/git/refs" -f ref=refs/tags/ai-reviewed -f sha="$to" >/dev/null
  fi
  echo "ai-reviewed -> $to"
else
  echo "ai-reviewed stays: $(jq -r 'join(", ")' <<<"$skipped") did not complete"
fi
