#!/usr/bin/env bash
# Merge Go coverprofiles (max count per block) and check per-package statement
# coverage against scripts/coverage-tiers.txt.
#
# COVER_ENFORCE=0 reports below-tier packages without failing.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tiers=${COVER_TIERS:-$root/scripts/coverage-tiers.txt}
enforce=${COVER_ENFORCE:-1}
module=$(awk '$1 == "module" { print $2; exit }' "$root/go.mod")

if [ "$#" -eq 0 ]; then
	echo "usage: $0 <coverprofile> [coverprofile...]" >&2
	exit 2
fi
[ -f "$tiers" ] || { echo "covcheck: missing tier table $tiers" >&2; exit 2; }

awk -v tierfile="$tiers" -v module="$module/" -v enforce="$enforce" '
function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); return s }
# Glob to ERE: `*` is the only wildcard; everything else is literal.
function globre(g,   r) {
	r = g
	gsub(/[.+?()\[\]{}^$|\\]/, "\\\\&", r)
	gsub(/\*/, ".*", r)
	return "^" r "$"
}
BEGIN {
	while ((getline line < tierfile) > 0) {
		sub(/#.*/, "", line)
		line = trim(line)
		if (line == "") continue
		split(line, f, /[ \t]+/)
		nt++
		tierPat[nt] = f[1]
		tierMin[nt] = f[2] + 0
		tierLiteral[nt] = (f[1] ~ /\*/) ? 0 : 1
	}
	close(tierfile)
}
/^mode:/ { next }
NF >= 3 {
	if (!($1 in stmts) || $3 + 0 > count[$1]) count[$1] = $3 + 0
	stmts[$1] = $2 + 0
}
END {
	for (block in stmts) {
		file = block
		sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", file)
		pkg = file
		sub(/\/[^\/]+$/, "", pkg)
		sub("^" module, "", pkg)
		total[pkg] += stmts[block]
		if (count[block] > 0) covered[pkg] += stmts[block]
		seen[pkg] = 1
	}

	# A tier naming a package with no instrumented statements still reports.
	for (i = 1; i <= nt; i++)
		if (tierLiteral[i] && !(tierPat[i] in seen)) { seen[tierPat[i]] = 1; total[tierPat[i]] = 0 }

	printf "%-34s %8s %8s %7s\n", "PACKAGE", "COVER", "TIER", "STATUS"
	n = 0
	for (pkg in seen) rows[++n] = pkg
	for (i = 1; i < n; i++)
		for (j = i + 1; j <= n; j++)
			if (rows[j] < rows[i]) { t = rows[i]; rows[i] = rows[j]; rows[j] = t }

	for (i = 1; i <= n; i++) {
		pkg = rows[i]
		min = -1
		for (k = 1; k <= nt; k++)
			if (pkg ~ globre(tierPat[k])) { min = tierMin[k]; break }
		if (min < 0) continue
		pct = (total[pkg] > 0) ? covered[pkg] * 100.0 / total[pkg] : 0
		status = (pct + 0.05 >= min) ? "ok" : "BELOW"
		if (status == "BELOW") failed++
		printf "%-34s %7.1f%% %7d%% %7s\n", pkg, pct, min, status
	}

	if (failed) {
		rc = (enforce == "0") ? 0 : 1
		printf "\ncovcheck: %d package(s) below tier%s\n", failed, rc ? "" : " (COVER_ENFORCE=0, not failing)"
		exit rc
	}
	print "\ncovcheck: every tiered package meets its floor"
}
' "$@"
