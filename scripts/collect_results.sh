#!/usr/bin/env bash
# Collect all five measurement conditions with the current codebase.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
TMP="$ROOT/tmp"
RESULTS="$ROOT/results"

ALL_SERVERS="localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055"
PEERS_1="localhost:50052,localhost:50053,localhost:50054,localhost:50055"
PEERS_2="localhost:50051,localhost:50053,localhost:50054,localhost:50055"
PEERS_3="localhost:50051,localhost:50052,localhost:50054,localhost:50055"
PEERS_4="localhost:50051,localhost:50052,localhost:50053,localhost:50055"
PEERS_5="localhost:50051,localhost:50052,localhost:50053,localhost:50054"

mkdir -p "$BIN" "$TMP" "$RESULTS"

echo "==> Building..."
go build -o "$BIN/server"  "$ROOT/cmd/server"
go build -o "$BIN/client"  "$ROOT/cmd/client"
go build -o "$BIN/loadgen" "$ROOT/cmd/loadgen"
echo "    Done."

kill_server() {
  local id=$1
  local pidfile="$TMP/server${id}.pid"
  if [ -f "$pidfile" ]; then
    kill -9 "$(cat "$pidfile")" 2>/dev/null || true
    rm -f "$pidfile"
  fi
}

kill_all() {
  for id in 1 2 3 4 5; do kill_server $id; done
  pkill -f "$BIN/server" 2>/dev/null || true
  sleep 0.5
}

start_server() {
  local id=$1 port=$2 peers=$3 dedup=${4:-true}
  "$BIN/server" --id "$id" --port "$port" --peers "$peers" --dedup="$dedup" \
    &>"$TMP/server${id}.log" &
  echo $! > "$TMP/server${id}.pid"
}

start_cluster() {
  local dedup="${1:-true}"
  start_server 1 50051 "$PEERS_1" "$dedup"
  start_server 2 50052 "$PEERS_2" "$dedup"
  start_server 3 50053 "$PEERS_3" "$dedup"
  start_server 4 50054 "$PEERS_4" "$dedup"
  start_server 5 50055 "$PEERS_5" "$dedup"
  local deadline=$((SECONDS + 15))
  while [ $SECONDS -lt $deadline ]; do
    "$BIN/client" --servers "$ALL_SERVERS" --counter _ready --increments 0 &>/dev/null && break
    sleep 0.3
  done
}

run() {
  local name=$1; shift
  echo ""
  echo "==> $name"
  "$BIN/loadgen" --servers "$ALL_SERVERS" --output "$RESULTS/${name}.csv" "$@"
  go run "$ROOT/scripts/analyze.go" --input "$RESULTS/${name}.csv"
}

kill_all

# ── 1. Baseline ───────────────────────────────────────────────────────────────
start_cluster true
run baseline \
  --counter baseline --workers 4 --increments 50 --think-time 20ms
kill_all

# ── 2. Planned failover (SIGTERM) ─────────────────────────────────────────────
start_cluster true
run planned_failover \
  --counter planned_failover --workers 4 --increments 50 --think-time 20ms \
  --kill-after 80 --kill-signal term --kill-target leader
kill_all

# ── 3. Hard crash dedup=ON ────────────────────────────────────────────────────
start_cluster true
run hard_crash_dedup_on \
  --counter hard_crash_dedup_on --workers 4 --increments 50 --think-time 20ms \
  --kill-after 80 --kill-signal kill --kill-target leader
kill_all

# ── 4. Hard crash dedup=OFF ───────────────────────────────────────────────────
start_cluster false
run hard_crash_dedup_off \
  --counter hard_crash_dedup_off --workers 4 --increments 50 --think-time 20ms \
  --kill-after 80 --kill-signal kill --kill-target leader
kill_all

# ── 5. Follower failure ───────────────────────────────────────────────────────
start_cluster true
run follower_failure \
  --counter follower_failure --workers 4 --increments 50 --think-time 20ms \
  --kill-after 80 --kill-signal kill --kill-target follower
kill_all

echo ""
echo "==> All done. Results in $RESULTS/"
