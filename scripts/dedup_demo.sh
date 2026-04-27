#!/usr/bin/env bash
# Focused dedup demo: guarantees the double-count window by injecting a
# 600ms delay on the leader between replication and ack, then SIGKILL-ing
# during that window. Run repeatedly to see consistent results.
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
TMP="$ROOT/tmp"
RESULTS="$ROOT/results"

PEERS_1="localhost:50052,localhost:50053,localhost:50054,localhost:50055"
PEERS_2="localhost:50051,localhost:50053,localhost:50054,localhost:50055"
PEERS_3="localhost:50051,localhost:50052,localhost:50054,localhost:50055"
PEERS_4="localhost:50051,localhost:50052,localhost:50053,localhost:50055"
PEERS_5="localhost:50051,localhost:50052,localhost:50053,localhost:50054"
ALL_SERVERS="localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055"

header() {
  echo ""
  echo -e "${BOLD}${BLUE}╔══════════════════════════════════════════════════════════╗${NC}"
  printf "${BOLD}${BLUE}║  %-56s║${NC}\n" "$1"
  echo -e "${BOLD}${BLUE}╚══════════════════════════════════════════════════════════╝${NC}"
  echo ""
}

step() { echo -e "${YELLOW}▶ $*${NC}"; }
info() { echo -e "  ${GREEN}✓ $*${NC}"; }
note() { echo -e "  $*"; }

json_field() {
  python3 -c "import json,sys; d=json.load(open('$1')); print(d['$2'])"
}

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
  local id=$1 port=$2 peers=$3 dedup=$4 delay=${5:-0}
  "$BIN/server" --id "$id" --port "$port" --peers "$peers" \
    --dedup="$dedup" --reply-delay="$delay" \
    &>"$TMP/server${id}.log" &
  echo $! > "$TMP/server${id}.pid"
}

wait_for_cluster() {
  local deadline=$((SECONDS + 15))
  while [ $SECONDS -lt $deadline ]; do
    if "$BIN/client" --servers "$ALL_SERVERS" --counter _ready --increments 0 &>/dev/null; then
      return 0
    fi
    sleep 0.3
  done
  echo "Cluster did not stabilize within 15s" >&2
  exit 1
}

start_cluster() {
  local dedup=$1 delay=${2:-0}
  start_server 1 50051 "$PEERS_1" "$dedup" "$delay"
  start_server 2 50052 "$PEERS_2" "$dedup" "$delay"
  start_server 3 50053 "$PEERS_3" "$dedup" "$delay"
  start_server 4 50054 "$PEERS_4" "$dedup" "$delay"
  start_server 5 50055 "$PEERS_5" "$dedup" "$delay"
  wait_for_cluster
}

mkdir -p "$BIN" "$TMP" "$RESULTS"
go build -o "$BIN/server"  "$ROOT/cmd/server"
go build -o "$BIN/client"  "$ROOT/cmd/client"
go build -o "$BIN/loadgen" "$ROOT/cmd/loadgen"
kill_all

# ── dedup=OFF ─────────────────────────────────────────────────────────────────

header "dedup=OFF  (expect double-count)"
note "Leader sleeps 600ms after replication but before ack."
note "Loadgen kills the leader mid-sleep → client retries on new leader"
note "→ new leader re-applies the increment → counter > successes."
echo ""

start_cluster false 600ms
info "Cluster up (dedup=false, reply-delay=600ms)"

step "Running loadgen: 1 worker × 20 ops, kill leader after op #10..."
echo ""
"$BIN/loadgen" \
  --servers "$ALL_SERVERS" \
  --counter dedup_test \
  --workers 1 \
  --increments 20 \
  --kill-after 10 \
  --kill-signal kill \
  --kill-target leader \
  --output "$RESULTS/dedup_demo_off.csv" || true
echo ""

OFF_SUCCESS=$(json_field "$RESULTS/dedup_demo_off.meta.json" success_count)
OFF_FINAL=$(json_field "$RESULTS/dedup_demo_off.meta.json" final_counter_value)
note "  success_count=${OFF_SUCCESS}   final_counter=${OFF_FINAL}"
if [ "$OFF_FINAL" -gt "$OFF_SUCCESS" ]; then
  echo -e "  ${RED}✗ Double-count confirmed: counter (${OFF_FINAL}) > successes (${OFF_SUCCESS})${NC}"
else
  echo -e "  ${YELLOW}  Window not hit (kill landed between ops). Try again.${NC}"
fi

kill_all

# ── dedup=ON ──────────────────────────────────────────────────────────────────

header "dedup=ON   (expect exactly-once)"
note "Same scenario, same delay — but the reply cache on the new leader"
note "recognises the retried requestID and returns the cached response."
echo ""

start_cluster true 600ms
info "Cluster up (dedup=true, reply-delay=600ms)"

step "Running loadgen: 1 worker × 20 ops, kill leader after op #10..."
echo ""
"$BIN/loadgen" \
  --servers "$ALL_SERVERS" \
  --counter dedup_test \
  --workers 1 \
  --increments 20 \
  --kill-after 10 \
  --kill-signal kill \
  --kill-target leader \
  --output "$RESULTS/dedup_demo_on.csv" || true
echo ""

ON_SUCCESS=$(json_field "$RESULTS/dedup_demo_on.meta.json" success_count)
ON_FINAL=$(json_field "$RESULTS/dedup_demo_on.meta.json" final_counter_value)
note "  success_count=${ON_SUCCESS}   final_counter=${ON_FINAL}"
if [ "$ON_FINAL" -eq "$ON_SUCCESS" ]; then
  echo -e "  ${GREEN}✓ Exactly-once confirmed: counter == successes${NC}"
else
  echo -e "  ${RED}✗ Unexpected divergence with dedup on — check logs${NC}"
fi

kill_all
info "Done."
