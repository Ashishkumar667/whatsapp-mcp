# Single image, two processes: the Go WhatsApp bridge (internal only, localhost)
# and the Python MCP server (the one thing exposed publicly). Built from the
# repo root so both whatsapp-bridge/ and whatsapp-mcp-server/ are in context -
# this is the one Dockerfile to point on-demand.io (or any single-Dockerfile
# PaaS) at.

FROM golang:1.26-bookworm AS bridge-builder
# go-sqlite3 needs cgo, which needs a real C toolchain.
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY whatsapp-bridge/go.mod whatsapp-bridge/go.sum ./
RUN go mod download
COPY whatsapp-bridge/ .
ENV CGO_ENABLED=1
RUN go build -o /out/whatsapp-bridge .

FROM python:3.12-slim AS mcp-deps
RUN pip install --no-cache-dir uv
WORKDIR /app/mcp-server
COPY whatsapp-mcp-server/pyproject.toml whatsapp-mcp-server/uv.lock ./
RUN uv sync --no-dev

FROM python:3.12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=bridge-builder /out/whatsapp-bridge ./bridge/whatsapp-bridge
COPY --from=mcp-deps /app/mcp-server/.venv ./mcp-server/.venv
COPY whatsapp-mcp-server/ ./mcp-server/
COPY docker/entrypoint.sh ./entrypoint.sh

RUN chmod +x ./entrypoint.sh ./bridge/whatsapp-bridge

ENV PATH="/app/mcp-server/.venv/bin:${PATH}"
ENV MCP_HOST=0.0.0.0
ENV MCP_PORT=8000

# Persisted across restarts: whatsmeow's per-device auth store (SQLite) and downloaded media.
VOLUME ["/app/bridge/store"]

EXPOSE 8000
ENTRYPOINT ["./entrypoint.sh"]
