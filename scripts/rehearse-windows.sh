#!/usr/bin/env bash
# Launch rehearsal for windowed runs (docs/lifecycle/windowed-runs/07-launch-review.md).
#
#   1. Build the previous release (PREV_REF, default main) and this candidate.
#   2. Generate DAYS days of data. The previous release ingests it and runs
#      a full reconciliation: a pre-existing database with a pre-existing run.
#   3. The candidate opens it (migration 0002 applies), verifies the old run,
#      runs full again (its digest must equal the old binary's), then runs
#      consecutive windows of each size in SIZES, reports and verifies each.
#   4. The union of each size's window reports must equal the full report
#      (run and outcome ids stripped, rows sorted), and score perfect against
#      the generator's ground truth.
#   5. Ingest cost of the new indexes: both binaries ingest the same files
#      into fresh databases. Timing of full vs one window of each size is
#      repeated REPEAT times; medians are reported.
#
# Every stage runs as its own process; wall, CPU and peak RSS come from
# RECOND_STATS=1 (getrusage), as in scripts/bench.sh.
#
#   scripts/rehearse-windows.sh                      # 100k payments, 28 days, windows of 7 and 1 days
#   N=20000 DAYS=14 SIZES="7 3 1" scripts/rehearse-windows.sh
set -euo pipefail
cd "$(dirname "$0")/.."

N=${N:-100000}
DAYS=${DAYS:-28}
SEED=${SEED:-1}
START=${START:-2026-10-01}
SIZES=${SIZES:-"7 1"}
REPEAT=${REPEAT:-3}
PREV_REF=${PREV_REF:-main}
out=${REHEARSAL_DIR:-rehearsal-out}

rm -rf "$out"
mkdir -p "$out/prev" "$out/logs"
go build -trimpath -o "$out/recond" ./cmd/recond
go build -trimpath -o "$out/gen" ./cmd/gen
prev_sha=$(git rev-parse --short "$PREV_REF")
git worktree add -q --detach "$out/prev-src" "$PREV_REF"
(cd "$out/prev-src" && go build -trimpath -o ../prev/recond ./cmd/recond)
git worktree remove --force "$out/prev-src"
cand_sha=$(git rev-parse --short HEAD)$(git diff --quiet HEAD || echo "+dirty")

stat() { sed -n 's/.*'"$2"'=\([^ ]*\).*/\1/p' "$1" | tail -1; }
# tosec DURATION: a Go duration string (1m2.5s, 3.5s, 850ms) in seconds
tosec() {
  awk -v d="$1" 'BEGIN {
    if (d ~ /ms$/) { sub(/ms$/, "", d); printf "%.3f\n", d / 1000; exit }
    t = 0
    if (d ~ /h/) { split(d, a, "h"); t += a[1] * 3600; d = a[2] }
    if (d ~ /m/) { split(d, a, "m"); t += a[1] * 60; d = a[2] }
    sub(/s$/, "", d); printf "%.3f\n", t + d
  }'
}
# mid VALUES...: the median (upper middle) of numbers with optional units
mid() { printf '%s\n' "$@" | sort -g | sed -n "$((($# + 1) / 2))p"; }
# timed NAME BINARY ARGS...: run one stage with stats; stdout to logs/NAME.out
timed() {
  local name=$1 bin=$2
  shift 2
  RECOND_STATS=1 "$bin" "$@" >"$out/logs/$name.out" 2>"$out/logs/$name.stats"
}
line() { printf '%-34s %10s %10s %12s  %s\n' "$@"; }
row() { line "$1" "$(stat "$out/logs/$1.stats" wall)" "$(stat "$out/logs/$1.stats" cpu)" "$(stat "$out/logs/$1.stats" peak_rss)" "${2:-}"; }
digest() { sed -n 's/^result digest //p' "$out/logs/$1.out"; }
rows() { tail -n +2 "$1" | cut -d, -f3- | LC_ALL=C sort; }   # drop run_id, outcome_id

echo "== environment"
echo "cpus: $(getconf _NPROCESSORS_ONLN 2>/dev/null || echo ?) ($(awk -F': ' '/model name/ {print $2; exit}' /proc/cpuinfo 2>/dev/null || echo ?))"
echo "ram: $(awk '/MemTotal/ {printf "%.0f GiB", $2/1048576}' /proc/meminfo 2>/dev/null || echo ?)"
echo "go: $(go version | awk '{print $3, $4}'), sqlite driver: $(awk '/modernc.org\/sqlite / {print $2}' go.mod)"
echo "previous release: $PREV_REF @ $prev_sha; candidate: $cand_sha"

"$out/gen" -out "$out/data" -n "$N" -seed "$SEED" -start "$START" -days "$DAYS" >"$out/logs/gen.out"
asof=$(awk '/reconcile as of/ {print $4}' "$out/logs/gen.out")
echo
echo "== data: $N payment attempts over $DAYS days from $START, seed $SEED, as of $asof"
head -2 "$out/logs/gen.out"

echo
echo "== upgrade: a database created by the previous release"
line stage wall cpu peak_rss note
export RECOND_DB="$out/recond.db"
unset RECOND_WINDOWED_RUNS
timed prev-ingest "$out/prev/recond" ingest -dir "$out/data"
row prev-ingest "previous release"
timed prev-full "$out/prev/recond" run -as-of "$asof"
row prev-full "previous release, run #1"
timed migrate "$out/recond" migrations
row migrate "candidate first open: migration 0002 (indexes built here)"
timed verify-prev "$out/recond" verify 1
row verify-prev "candidate verifies the pre-existing run: $(grep -q MISMATCH "$out/logs/verify-prev.out" && echo MISMATCH || echo reproduced)"
timed full "$out/recond" run -as-of "$asof"
same=$([ "$(digest full)" = "$(digest prev-full)" ] && echo "identical to run #1" || echo "DIFFERS from run #1")
row full "candidate full run #2, digest $same"
timed full-report "$out/recond" report -out "$out/reports"
timed full-verify "$out/recond" verify latest
row full-verify "$(grep -q MISMATCH "$out/logs/full-verify.out" && echo MISMATCH || echo reproduced)"
full_run=$(sed -n 's/^run #\([0-9]*\).*/\1/p' "$out/logs/full.out")
rows "$out/reports/run-$full_run/items.csv" >"$out/full.rows"

echo
echo "== consecutive windows vs the full run (RECOND_WINDOWED_RUNS=on, default margins)"
export RECOND_WINDOWED_RUNS=on
end=$(date -u -d "$START + $DAYS days" +%F)
printf '%-6s %8s %12s %10s %14s %8s %10s %12s\n' size windows rows_differ score outside_window verify max_wall max_peak_rss
for size in $SIZES; do
  from=$START
  items=() n=0 outside=0 verified=0 maxwall=0 maxrss=0
  : >"$out/size$size.rows"
  while [[ "$from" < "$end" || "$from" == "$end" ]]; do
    to=$(date -u -d "$from + $((size - 1)) days" +%F)
    name="w$size-$from"
    timed "$name" "$out/recond" run -as-of "$asof" -from "$from" -to "$to"
    id=$(sed -n 's/^run #\([0-9]*\).*/\1/p' "$out/logs/$name.out")
    "$out/recond" report -out "$out/reports" "$id" >/dev/null
    items+=(-items "$out/reports/run-$id/items.csv")
    rows "$out/reports/run-$id/items.csv" >>"$out/size$size.rows"
    outside=$((outside + $(sed -n 's/.*outside_window (counterpart beyond the margin): \([0-9,]*\).*/\1/p' "$out/logs/$name.out" | tr -d ,)))
    if "$out/recond" verify "$id" >"$out/logs/$name.verify" 2>&1; then verified=$((verified + 1)); fi
    w=$(tosec "$(stat "$out/logs/$name.stats" wall)")
    r=$(stat "$out/logs/$name.stats" peak_rss)
    maxwall=$(awk -v a="$maxwall" -v b="$w" 'BEGIN { print (b + 0 > a + 0) ? b : a }')
    maxrss=$(awk -v a="$maxrss" -v b="${r%MiB}" 'BEGIN { print (b + 0 > a + 0) ? b : a }')
    n=$((n + 1))
    from=$(date -u -d "$from + $size days" +%F)
  done
  LC_ALL=C sort -o "$out/size$size.rows" "$out/size$size.rows"
  differ=$(diff "$out/full.rows" "$out/size$size.rows" | grep -c '^[<>]' || true)
  score=$("$out/gen" score -truth "$out/data/truth.json" "${items[@]}" >"$out/logs/score$size.out" 2>&1 && echo perfect || echo FAILED)
  printf '%-6s %8s %12s %10s %14s %8s %10s %12s\n' "$size" "$n" "$differ" "$score" "$outside" "$verified/$n" "${maxwall}s" "${maxrss}MiB"
done
echo "(full report: $(wc -l <"$out/full.rows") rows)"

echo
echo "== cost: full run vs one window, median of $REPEAT"
line stage wall cpu peak_rss "loaded (window block)"
middle=$(date -u -d "$START + $((DAYS / 2)) days" +%F)
for what in full $SIZES; do
  for i in $(seq "$REPEAT"); do
    if [ "$what" = full ]; then
      RECOND_WINDOWED_RUNS=off timed "cost-full-$i" "$out/recond" run -as-of "$asof"
    else
      timed "cost-w$what-$i" "$out/recond" run -as-of "$asof" -from "$middle" -to "$(date -u -d "$middle + $((what - 1)) days" +%F)"
    fi
  done
  label=$([ "$what" = full ] && echo full || echo "window ${what}d")
  base=$([ "$what" = full ] && echo cost-full || echo "cost-w$what")
  walls=() rss=() cpus=()
  for i in $(seq "$REPEAT"); do
    walls+=("$(tosec "$(stat "$out/logs/$base-$i.stats" wall)")")
    cpus+=("$(tosec "$(stat "$out/logs/$base-$i.stats" cpu)")")
    rss+=("$(stat "$out/logs/$base-$i.stats" peak_rss)")
  done
  loaded=$(sed -n 's/.*dated in the window: \(.*\)/\1/p' "$out/logs/$base-1.out")
  [ -z "$loaded" ] && loaded="$(sed -n 's/^inputs: \(.* ledger txns\), \(.* settlement lines\), .*/\1, \2/p' "$out/logs/$base-1.out")"
  line "$label" "$(mid "${walls[@]}")s" "$(mid "${cpus[@]}")s" "$(mid "${rss[@]}")" "$loaded"
  printf '  each run: wall %ss | peak %s\n' "${walls[*]}" "${rss[*]}"
done

echo
echo "== ingest cost of the 0002 indexes (same files, fresh databases)"
line stage wall cpu peak_rss
RECOND_DB="$out/prev-fresh.db" timed ingest-prev "$out/prev/recond" ingest -dir "$out/data"
row ingest-prev
RECOND_DB="$out/cand-fresh.db" timed ingest-cand "$out/recond" ingest -dir "$out/data"
row ingest-cand
rm -f "$out"/prev-fresh.db* "$out"/cand-fresh.db*
echo "database: $(du -h "$out/recond.db" | cut -f1)"
echo
echo "logs and reports are in $out/"
