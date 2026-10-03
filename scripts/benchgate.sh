#!/usr/bin/env bash
# Allocation gate over `go test -bench . -benchmem` output read on stdin.
#
#   benchgate.sh --baseline <file>              write "<pkg>/<Benchmark> <allocs> [<tolerance>]"
#   benchgate.sh --check <file> [--tolerance N] fail on an allocs/op regression
#
# --tolerance applies to entries without a tolerance column of their own.
#
# Only allocs/op is gated; ns/op is too noisy on shared CI runners. Repeated
# measurements of the same benchmark collapse to their minimum.
set -euo pipefail

mode=""
file=""
tolerance=${BENCH_ALLOC_TOLERANCE:-0}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--baseline | --check)
		mode=${1#--}
		file=$2
		shift 2
		;;
	--tolerance)
		tolerance=$2
		shift 2
		;;
	*)
		echo "benchgate: unknown argument $1" >&2
		exit 2
		;;
	esac
done

if [ -z "$mode" ] || [ -z "$file" ]; then
	echo "usage: $0 --baseline <file> | --check <file> [--tolerance N]" >&2
	exit 2
fi
[ "$mode" = "baseline" ] || [ -f "$file" ] || {
	echo "benchgate: missing baseline $file — run 'make bench-baseline'" >&2
	exit 2
}

# min allocs/op per "<module-relative pkg>/<BenchmarkName>".
module=$(awk '$1 == "module" { print $2; exit }' "$(cd "$(dirname "$0")/.." && pwd)/go.mod")

mins=$(awk -v module="$module/" '
	/^pkg:[ \t]/ { pkg = $2; sub("^" module, "", pkg); next }
	/allocs\/op/ && $1 ~ /^Benchmark/ {
		name = $1
		sub(/-[0-9]+$/, "", name)
		for (i = 1; i <= NF; i++)
			if ($i == "allocs/op") a = $(i - 1) + 0
		key = pkg "/" name
		if (!(key in best) || a < best[key]) best[key] = a
	}
	END { for (k in best) printf "%s %d\n", k, best[k] }
' | sort)

if [ -z "$mins" ]; then
	echo "benchgate: no benchmark results on stdin" >&2
	exit 2
fi

if [ "$mode" = "baseline" ]; then
	# Regenerating keeps each entry's own tolerance (the optional third column).
	entries=$(printf '%s\n' "$mins" | awk -v old="$file" '
		BEGIN {
			while ((getline line < old) > 0) {
				sub(/#.*/, "", line)
				if (split(line, f, /[ \t]+/) >= 3) own[f[1]] = f[3]
			}
		}
		{ print $0 (($1 in own) ? " " own[$1] : "") }
	')
	{
		echo "# allocs/op per benchmark, the floor bench-gate compares against."
		echo "# An optional third column is that entry's tolerance in percent, for"
		echo "# benchmarks whose count varies run to run (map growth)."
		echo "# Regenerate with 'make bench-baseline' after an intentional change."
		echo "$entries"
	} >"$file"
	echo "benchgate: wrote $(printf '%s\n' "$mins" | wc -l | tr -d ' ') entries to $file"
	exit 0
fi

printf '%s\n' "$mins" | awk -v baseline="$file" -v tol="$tolerance" '
BEGIN {
	while ((getline line < baseline) > 0) {
		sub(/#.*/, "", line)
		if (line ~ /^[ \t]*$/) continue
		if (split(line, f, /[ \t]+/) >= 3) own[f[1]] = f[3] + 0
		base[f[1]] = f[2] + 0
	}
	close(baseline)
	printf "%-58s %8s %8s %7s\n", "BENCHMARK", "ALLOCS", "BASE", "STATUS"
}
{
	if (!($1 in base)) { printf "%-58s %8d %8s %7s\n", $1, $2, "-", "new"; next }
	seen[$1] = 1
	limit = base[$1] * (100 + (($1 in own) ? own[$1] : tol))
	status = ($2 * 100 <= limit) ? "ok" : "REGRESS"
	if (status == "REGRESS") failed++
	printf "%-58s %8d %8d %7s\n", $1, $2, base[$1], status
}
END {
	for (k in base)
		if (!(k in seen)) printf "%-58s %8s %8d %7s\n", k, "-", base[k], "gone"
	if (failed) {
		printf "\nbench-gate: %d benchmark(s) over the %d%% allocs/op tolerance\n", failed, tol
		exit 1
	}
	printf "\nbench-gate: allocs/op within %d%% of the baseline\n", tol
}
'
