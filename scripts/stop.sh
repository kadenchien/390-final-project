#!/usr/bin/env bash
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
pkill -f "$ROOT/bin/server" 2>/dev/null && echo "Servers stopped." || echo "No servers running."
