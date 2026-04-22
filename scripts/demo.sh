#!/usr/bin/env bash
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"

echo "==> Building..."
mkdir -p "$BIN"
go build -o "$BIN/server" "$ROOT/cmd/server"
go build -o "$BIN/client" "$ROOT/cmd/client"
echo "    Done."

# Kill any leftover servers from a previous run (compiled binary or go run)
pkill -f "$BIN/server"        2>/dev/null || true
pkill -f "go run.*cmd/server" 2>/dev/null || true
pkill -f "cmd/server"         2>/dev/null || true
# Also kill by port to catch go run temp binaries that survive the above
for port in 50051 50052 50053 50054 50055; do
  pids=$(lsof -ti tcp:$port -s tcp:LISTEN 2>/dev/null) || true
  [ -n "$pids" ] && kill -9 $pids 2>/dev/null || true
done
sleep 1

PEERS_1="localhost:50052,localhost:50053,localhost:50054,localhost:50055"
PEERS_2="localhost:50051,localhost:50053,localhost:50054,localhost:50055"
PEERS_3="localhost:50051,localhost:50052,localhost:50054,localhost:50055"
PEERS_4="localhost:50051,localhost:50052,localhost:50053,localhost:50055"
PEERS_5="localhost:50051,localhost:50052,localhost:50053,localhost:50054"

echo "==> Starting 5 servers..."
"$BIN/server" --id 1 --port 50051 --peers "$PEERS_1" &> /tmp/server1.log &
"$BIN/server" --id 2 --port 50052 --peers "$PEERS_2" &> /tmp/server2.log &
"$BIN/server" --id 3 --port 50053 --peers "$PEERS_3" &> /tmp/server3.log &
"$BIN/server" --id 4 --port 50054 --peers "$PEERS_4" &> /tmp/server4.log &
"$BIN/server" --id 5 --port 50055 --peers "$PEERS_5" &> /tmp/server5.log &

sleep 1
echo "    Servers running. Initial leader: localhost:50051 (view 0)"
echo ""
echo "==> Streaming 60 increments (300ms apart); leader will be killed after increment 40..."
echo ""

"$BIN/client" \
  --servers "localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055" \
  --counter demo \
  --increments 60 \
  --sleep 300ms &
CLIENT_PID=$!

# Kill the leader partway through (~40 increments in)
sleep 12
echo ""
echo "==> Killing leader (localhost:50051)..."
kill -9 $(lsof -ti tcp:50051 -s tcp:LISTEN) 2>/dev/null || true

wait $CLIENT_PID
