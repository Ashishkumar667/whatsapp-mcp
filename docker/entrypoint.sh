#!/bin/bash
set -euo pipefail

: "${INTERNAL_API_SECRET:?INTERNAL_API_SECRET must be set}"
: "${MONGODB_URI:?MONGODB_URI must be set}"

BRIDGE_PORT="${BRIDGE_PORT:-8080}"
export PORT="$BRIDGE_PORT"
export WHATSAPP_BRIDGE_URL="http://127.0.0.1:${BRIDGE_PORT}"

# The Go bridge only ever listens on localhost inside this container - it is
# never reached directly, only through the Python MCP server below.
cd /app/bridge
./whatsapp-bridge &
BRIDGE_PID=$!

cleanup() {
  kill "$BRIDGE_PID" "${MCP_PID:-}" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "Waiting for the WhatsApp bridge to come up..."
for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:${BRIDGE_PORT}/healthz" >/dev/null 2>&1; then
    echo "Bridge is up."
    break
  fi
  sleep 1
done

cd /app/mcp-server
python main.py &
MCP_PID=$!

wait "$MCP_PID"
