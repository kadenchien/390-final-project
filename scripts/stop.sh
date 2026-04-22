#!/usr/bin/env bash
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
killed=0

# Kill by process name (compiled binary or go run wrapper)
pkill -f "$ROOT/bin/server"    2>/dev/null && killed=1 || true
pkill -f "go run.*cmd/server"  2>/dev/null && killed=1 || true
pkill -f "cmd/server"          2>/dev/null && killed=1 || true

# Kill whatever is still holding the demo ports (catches go run temp binaries)
for port in 50051 50052 50053 50054 50055; do
  pids=$(lsof -ti tcp:$port -s tcp:LISTEN 2>/dev/null) || true
  if [ -n "$pids" ]; then
    kill -9 $pids 2>/dev/null && killed=1 || true
  fi
done

if [ $killed -eq 1 ]; then echo "Servers stopped."; else echo "No servers running."; fi
