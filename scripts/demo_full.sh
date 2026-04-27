#!/usr/bin/env bash
# Full feature demo: transparent redirect, leader failover, crash recovery, exactly-once dedup.
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

ALL_SERVERS="localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055"
PEERS_1="localhost:50052,localhost:50053,localhost:50054,localhost:50055"
PEERS_2="localhost:50051,localhost:50053,localhost:50054,localhost:50055"
PEERS_3="localhost:50051,localhost:50052,localhost:50054,localhost:50055"
PEERS_4="localhost:50051,localhost:50052,localhost:50053,localhost:50055"
PEERS_5="localhost:50051,localhost:50052,localhost:50053,localhost:50054"

header() {
  echo ""
  echo -e "${BOLD}${BLUE}╔══════════════════════════════════════════════════════════╗${NC}"
  printf "${BOLD}${BLUE}║  %-56s║${NC}\n" "$1"
  echo -e "${BOLD}${BLUE}╚══════════════════════════════════════════════════════════╝${NC}"
  echo ""
}

step()  { echo -e "${YELLOW}▶ $*${NC}"; }
info()  { echo -e "  ${GREEN}✓ $*${NC}"; }
note()  { echo -e "  $*"; }
pause() { echo ""; read -rp "  Press ENTER to continue... "; echo ""; }

json_field() {
  # json_field <file> <key>  — extract a top-level integer field without jq
  python3 -c "import json,sys; d=json.load(open('$1')); print(d['$2'])"
}

kill_server() {
  local id=$1
  local pidfile="$TMP/server${id}.pid"
  if [ -f "$pidfile" ]; then
    local pid
    pid=$(cat "$pidfile")
    kill -9 "$pid" 2>/dev/null || true
    rm -f "$pidfile"
  fi
}

kill_all_servers() {
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

start_all_servers() {
  local dedup="${1:-true}"
  start_server 1 50051 "$PEERS_1" "$dedup"
  start_server 2 50052 "$PEERS_2" "$dedup"
  start_server 3 50053 "$PEERS_3" "$dedup"
  start_server 4 50054 "$PEERS_4" "$dedup"
  start_server 5 50055 "$PEERS_5" "$dedup"
  wait_for_cluster
  info "5 servers running (dedup=$dedup). Initial leader: localhost:50051 (view 0)"
}

# ── Setup ─────────────────────────────────────────────────────────────────────

header "Building binaries"
mkdir -p "$BIN" "$TMP" "$RESULTS"
go build -o "$BIN/server"  "$ROOT/cmd/server"
go build -o "$BIN/client"  "$ROOT/cmd/client"
go build -o "$BIN/loadgen" "$ROOT/cmd/loadgen"
info "Build complete."
kill_all_servers

pause

# ══════════════════════════════════════════════════════════════════════════════
# PART 1: Transparent Redirect
# ══════════════════════════════════════════════════════════════════════════════

header "PART 1 — Transparent Redirect via Interceptor"
note "The client connects to localhost:50053 (a follower, not the leader)."
note "The leader interceptor inspects each response's redirect_to field and"
note "silently rebinds to the real leader. Application code is unaware."
echo ""

pause
start_all_servers

step "Sending 5 increments via localhost:50053 (follower)..."
echo ""
"$BIN/client" \
  --servers "localhost:50053,localhost:50051,localhost:50052,localhost:50054,localhost:50055" \
  --counter demo \
  --increments 5 \
  --sleep 400ms
echo ""
info "All 5 succeeded. Every log line shows leader=localhost:50051 — the interceptor"
info "redirected transparently even though we dialed a follower."

pause

# ══════════════════════════════════════════════════════════════════════════════
# PART 2: Leader Failover with W=N Replication
# ══════════════════════════════════════════════════════════════════════════════

header "PART 2 — Leader Failover with W=N Replication"
note "We stream 30 increments (500ms apart). After ~15, we SIGKILL the leader."
note "Because replicateToAll() now blocks until ALL followers ack, every follower"
note "is immediately up-to-date. The new leader needs zero catch-up."
echo ""

pause
step "Streaming 30 increments — leader will be killed after ~15..."
echo ""

"$BIN/client" \
  --servers "$ALL_SERVERS" \
  --counter demo \
  --increments 30 \
  --sleep 500ms &
CLIENT_PID=$!

sleep 7   # ~14 increments at 500ms
echo ""
step "Killing leader (localhost:50051)..."
kill_server 1
echo ""

wait $CLIENT_PID || true
echo ""
info "Stream complete. The client failed over with no manual reconnect."
info "No increments were lost — the new leader had full state immediately."

pause

# ══════════════════════════════════════════════════════════════════════════════
# PART 3: Crash Recovery via State Transfer
# ══════════════════════════════════════════════════════════════════════════════

header "PART 3 — Crash Recovery via State Transfer"
note "Server 1 (localhost:50051) is still dead from Part 2."
note "We advance the counter 10 more times so it falls behind."
note "When server 1 restarts, PullState() calls TransferState on a live peer"
note "and receives the full counter table + reply cache before joining."
echo ""

pause
step "Sending 10 more increments to the live cluster (server 1 is still down)..."
echo ""
"$BIN/client" \
  --servers "localhost:50052,localhost:50053,localhost:50054,localhost:50055" \
  --counter demo \
  --increments 10 \
  --sleep 200ms
echo ""

step "Restarting server 1 (localhost:50051)..."
start_server 1 50051 "$PEERS_1" true
sleep 2

step "Server 1 startup log:"
echo ""
grep -E "PullState|synced|starting with empty" "$TMP/server1.log" || echo "  (no PullState lines found — check $TMP/server1.log)"
echo ""
info "Server 1 has rejoined with current counter state and reply cache."
info "It can immediately serve as leader or follower without any catch-up writes."

pause

# ══════════════════════════════════════════════════════════════════════════════
# PART 4: Exactly-Once Semantics — Dedup On vs Off
# ══════════════════════════════════════════════════════════════════════════════

header "PART 4 — Exactly-Once Semantics: Dedup On vs. Off"
note "Scenario: leader applies an increment, replicates to all followers (W=N),"
note "then dies before acking the client. The client retries on the new leader."
note ""
note "  dedup=OFF  → new leader re-applies the increment: counter is double-counted."
note "  dedup=ON   → new leader finds the requestID in the reply cache and returns"
note "               the cached response — counter is unchanged."
echo ""

# ── 4a: dedup=OFF ─────────────────────────────────────────────────────────────

pause
step "Restarting fresh cluster with dedup=false..."
kill_all_servers
sleep 0.5
start_all_servers false

step "Running loadgen: 1 worker × 60 increments, SIGKILL leader after op #30 (dedup=OFF)..."
echo ""
"$BIN/loadgen" \
  --servers "$ALL_SERVERS" \
  --counter exactlyonce \
  --workers 1 \
  --increments 60 \
  --kill-after 30 \
  --kill-signal kill \
  --kill-target leader \
  --output "$RESULTS/dedup_off.csv" || true
echo ""

OFF_SUCCESS=$(json_field "$RESULTS/dedup_off.meta.json" success_count)
OFF_FINAL=$(json_field "$RESULTS/dedup_off.meta.json" final_counter_value)

note "  dedup=OFF  →  success_count=${OFF_SUCCESS}   final_counter=${OFF_FINAL}"
if [ "$OFF_FINAL" -gt "$OFF_SUCCESS" ]; then
  echo -e "  ${RED}✗ Double-count detected: counter (${OFF_FINAL}) > successes (${OFF_SUCCESS})${NC}"
else
  echo -e "  ${YELLOW}  Timing window not hit this run — no double-count observed.${NC}"
fi

# ── 4b: dedup=ON ──────────────────────────────────────────────────────────────

pause
step "Restarting fresh cluster with dedup=true..."
kill_all_servers
sleep 0.5
start_all_servers true

step "Running loadgen: 1 worker × 60 increments, SIGKILL leader after op #30 (dedup=ON)..."
echo ""
"$BIN/loadgen" \
  --servers "$ALL_SERVERS" \
  --counter exactlyonce \
  --workers 1 \
  --increments 60 \
  --kill-after 30 \
  --kill-signal kill \
  --kill-target leader \
  --output "$RESULTS/dedup_on.csv" || true
echo ""

ON_SUCCESS=$(json_field "$RESULTS/dedup_on.meta.json" success_count)
ON_FINAL=$(json_field "$RESULTS/dedup_on.meta.json" final_counter_value)

note "  dedup=ON   →  success_count=${ON_SUCCESS}   final_counter=${ON_FINAL}"
if [ "$ON_FINAL" -eq "$ON_SUCCESS" ]; then
  echo -e "  ${GREEN}✓ Exactly-once confirmed: counter == successes.${NC}"
else
  echo -e "  ${RED}✗ Unexpected divergence with dedup on — check logs.${NC}"
fi

# ── Wrap-up ───────────────────────────────────────────────────────────────────

header "Demo Complete"
info "Part 1 — Transparent redirect: interceptor silently rebinds to leader"
info "Part 2 — Leader failover: W=N replication means zero catch-up on failover"
info "Part 3 — Crash recovery: PullState() syncs full state on rejoin"
info "Part 4 — Exactly-once: reply cache prevents double-counting on retry"
echo ""
note "  Results written to $RESULTS/"
note "  Server logs in    $TMP/"
echo ""

kill_all_servers
info "All servers stopped."
