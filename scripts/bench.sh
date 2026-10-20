#!/usr/bin/env bash
# End-to-end benchmark: gen -> ingest -> run -> report -> verify -> score,
# one fresh database per size. Wall time, CPU time and peak RSS per stage come
# from RECOND_STATS=1 (getrusage), so each stage is measured in its own process.
#
#   scripts/bench.sh                 # 100k payment attempts
#   scripts/bench.sh 100000 1000000  # what the README reports
set -euo pipefail
cd "$(dirname "$0")/.."

sizes=("$@")
[ ${#sizes[@]} -eq 0 ] && sizes=(100000)
out=${BENCH_DIR:-bench-out}
mkdir -p "$out"
go build -trimpath -o "$out/recond" ./cmd/recond
go build -trimpath -o "$out/gen" ./cmd/gen

cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo "?")
mem=$(awk '/MemTotal/ {printf "%.0f GiB", $2/1048576}' /proc/meminfo 2>/dev/null || echo "?")
echo "environment: ${cpus} CPUs, ${mem} RAM, $(go version | awk '{print $3, $4}')"

stat() { # stat <file> <field>  ->  value of field=... on the stats line
  sed -n 's/.*'"$2"'=\([^ ]*\).*/\1/p' "$1" | tail -1
}

for n in "${sizes[@]}"; do
  dir="$out/n$n"
  rm -rf "$dir"
  mkdir -p "$dir"
  "$out/gen" -out "$dir/data" -n "$n" -seed 1 -days 7 >"$dir/gen.txt"
  asof=$(awk '/reconcile as of/ {print $4}' "$dir/gen.txt")
  export RECOND_DB="$dir/recond.db" RECOND_STATS=1
  "$out/recond" ingest -dir "$dir/data" >"$dir/ingest.txt" 2>"$dir/ingest.stats"
  "$out/recond" run -as-of "$asof" >"$dir/run.txt" 2>"$dir/run.stats"
  "$out/recond" report -out "$dir/reports" >"$dir/report.txt" 2>"$dir/report.stats"
  "$out/recond" verify >"$dir/verify.txt" 2>"$dir/verify.stats"
  "$out/gen" score -truth "$dir/data/truth.json" -items "$dir/reports/run-1/items.csv" >"$dir/score.txt" || true
  unset RECOND_DB RECOND_STATS

  echo
  echo "== $n payment attempts"
  head -1 "$dir/gen.txt"
  sed -n 's/^inputs: /inputs: /p' "$dir/run.txt"
  printf '%-8s %10s %10s %12s\n' stage wall cpu peak_rss
  for s in ingest run report verify; do
    printf '%-8s %10s %10s %12s\n' "$s" "$(stat "$dir/$s.stats" wall)" "$(stat "$dir/$s.stats" cpu)" "$(stat "$dir/$s.stats" peak_rss)"
  done
  grep '^timings:' "$dir/run.txt" || true
  grep -E 'replayed in' "$dir/verify.txt" || true
  echo "database: $(du -h "$dir/recond.db" | cut -f1)"
  tail -2 "$dir/score.txt"
done
