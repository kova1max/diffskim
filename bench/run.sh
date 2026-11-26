#!/usr/bin/env bash
# Measures diffskim against the git built-ins baseline on the PRs in
# bench/prs.tsv and writes bench/results.tsv.
#
#   raw       changed lines of  git diff -M base head
#   baseline  changed lines of  git diff -M -w base head -- . <lockfile excludes>
#   kept      changed lines of  diffskim base head
#
# Every count comes from git's own --numstat, not from diffskim's parser,
# and every diffskim output is checked with git apply --check against base.
# Needs git, go and network access the first time; repositories are cached
# as blobless partial clones in $DIFFSKIM_BENCH_CACHE.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cache=${DIFFSKIM_BENCH_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/diffskim-bench}
bin=$(mktemp -d)/diffskim
trap 'rm -rf "$(dirname "$bin")"' EXIT
(cd "$root" && go build -o "$bin" .)

# The baseline excludes the same lockfile names diffskim folds.
excludes=()
while read -r name; do
	excludes+=(":(exclude,glob)**/$name")
done < <("$bin" --lockfiles)

# sum_numstat adds up added+removed lines of `git diff --numstat` or
# `git apply --numstat` output; binary files ("-") count as zero.
sum_numstat() { awk '$1 != "-" { n += $1 + $2 } END { print n + 0 }'; }

out="$root/bench/results.tsv"
printf 'pr\traw\tbaseline\tkept\tkept_vs_baseline\tlockfile\tgenerated\twhitespace-only\timport-only\tformat-only\n' > "$out.tmp"
grep -v '^#' "$root/bench/prs.tsv" | while IFS=$'\t' read -r repo pr base head; do
	dir="$cache/${repo//\//__}"
	if [ ! -d "$dir/.git" ]; then
		git init -q "$dir"
		git -C "$dir" remote add origin "https://github.com/$repo.git"
	fi
	if ! git -C "$dir" cat-file -e "$base^{commit}" 2>/dev/null || ! git -C "$dir" cat-file -e "$head^{commit}" 2>/dev/null; then
		git -C "$dir" fetch -q --filter=blob:none --depth=1 origin "$base" "$head"
	fi
	g() { git -C "$dir" -c core.quotePath=false "$@"; }
	raw=$(g diff -M --numstat "$base" "$head" | sum_numstat)
	baseline=$(g diff -M -w --numstat "$base" "$head" -- . "${excludes[@]}" | sum_numstat)
	skimmed=$(mktemp)
	(cd "$dir" && "$bin" -q "$base" "$head" < /dev/null > "$skimmed")
	kept=$(git -C "$dir" apply --numstat "$skimmed" | sum_numstat)
	index=$(mktemp)
	rm -f "$index"
	GIT_INDEX_FILE="$index" g read-tree "$base"
	if ! GIT_INDEX_FILE="$index" g apply --cached --check "$skimmed"; then
		echo "bench: diffskim output for $repo#$pr does not apply to its base" >&2
		exit 1
	fi
	rm -f "$index" "$skimmed"
	folded=$(cd "$dir" && "$bin" --json "$base" "$head" < /dev/null |
		python3 -c 'import json,sys; f=json.load(sys.stdin)["folded"]; print("\t".join(str(f.get(r,0)) for r in ["lockfile","generated","whitespace-only","import-only","format-only"]))')
	pct=$(awk -v b="$baseline" -v k="$kept" 'BEGIN { if (b == 0) print "n/a"; else printf "%+.1f%%\n", (k - b) * 100 / b }')
	printf '%s#%s\t%s\t%s\t%s\t%s\t%s\n' "$repo" "$pr" "$raw" "$baseline" "$kept" "$pct" "$folded" >> "$out.tmp"
	echo "$repo#$pr raw=$raw baseline=$baseline kept=$kept ($pct)" >&2
done
mv "$out.tmp" "$out"
python3 "$root/bench/summarize.py" "$out"
