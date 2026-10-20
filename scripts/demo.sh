#!/usr/bin/env bash
# A guided tour of recond on synthetic data: generate a world with injected
# faults, ingest it (twice - the second time is a no-op), reconcile, score the
# report against the generator's ground truth, replay the run, and work one
# exception through the audit trail.
set -euo pipefail
cd "$(dirname "$0")/.."

out=${DEMO_DIR:-demo-out}
rm -rf "$out"
mkdir -p "$out"
go build -o "$out/recond" ./cmd/recond
go build -o "$out/gen" ./cmd/gen
export RECOND_DB="$out/recond.db"

step() {
  printf '\n$ %s\n' "$*"
  "$@"
}

# step_head N cmd...: run cmd, show only its first N lines (no SIGPIPE under pipefail)
step_head() {
  local n=$1
  shift
  printf '\n$ %s | head -%s\n' "$*" "$n"
  "$@" >"$out/.step"
  head -n "$n" "$out/.step"
}

# step_tail N cmd...: run cmd, show only its last N lines
step_tail() {
  local n=$1
  shift
  printf '\n$ %s | tail -%s\n' "$*" "$n"
  "$@" >"$out/.step"
  tail -n "$n" "$out/.step"
}

step "$out/gen" -out "$out/data" -n 5000 -seed 42 | tee "$out/gen.txt"
asof=$(awk '/reconcile as of/ {print $4}' "$out/gen.txt")

step "$out/recond" ingest -dir "$out/data"
step_tail 3 "$out/recond" ingest -dir "$out/data"   # same files again: nothing changes
step "$out/recond" run -as-of "$asof"
step_tail 2 "$out/recond" report -out "$out/reports"
step "$out/gen" score -truth "$out/data/truth.json" -items "$out/reports/run-1/items.csv"
step "$out/recond" verify 1
step "$out/recond" exceptions -class fee_mismatch -limit 3

id=$("$out/recond" exceptions -class fee_mismatch -json -limit 1 | sed -n 's/^\[{"id":\([0-9]*\).*/\1/p')
step_head 30 "$out/recond" exceptions show "$id"
step "$out/recond" resolve "$id" -by demo.operator -reason "provider confirmed fee correction in next cycle"
step_tail 3 "$out/recond" run -as-of "$asof"   # rerun: the human decision stands
step_tail 4 "$out/recond" exceptions show "$id"
step "$out/recond" runs

rm -f "$out/.step"
printf '\ndemo data, database and reports are in %s/\n' "$out"
