# WhatsApp MCP Server

This is a multi-tenant Model Context Protocol (MCP) server for WhatsApp, served over **streamable HTTP** so it can be hosted and shared by many users at once - each user links their own personal WhatsApp account and only ever sees their own chats.

With this you can search and read your personal WhatsApp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to WhatsApp directly via the WhatsApp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). Chat history is stored in MongoDB, scoped per user, and only sent to an LLM (such as Claude) when the agent accesses it through tools (which you control).

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that prompt injection could lead to private data exfiltration.

## Architecture

Two services, plus MongoDB:

1. **Go WhatsApp bridge** (`whatsapp-bridge/`): manages one `whatsmeow` device per linked WhatsApp account ("session"), handles QR pairing, and keeps chats/messages in MongoDB up to date. It exposes an **internal-only** REST API (not for direct public access) that the MCP server uses to start pairing and to send/download media on behalf of a specific session.
2. **Python MCP server** (`whatsapp-mcp-server/`): the public-facing streamable-HTTP MCP endpoint. Resolves each request's `Authorization: Bearer <api_token>` header to a `session_id` (looked up in MongoDB) and scopes every tool call to that user's data.
3. **MongoDB**: the shared app data store - a `sessions` registry (which WhatsApp account maps to which API token) plus per-session `chats` and `messages` collections. (whatsmeow's own per-device auth/crypto material stays in a SQLite file on the bridge's persistent volume - that's opaque session state, not something callers ever query.)

```
Claude / any MCP client
        │  Authorization: Bearer <api_token>  (streamable HTTP)
        ▼
whatsapp-mcp-server  ──────────────►  MongoDB (sessions, chats, messages)
        │  X-Internal-Secret (private network)
        ▼
whatsapp-bridge  ──────────────────►  WhatsApp (via whatsmeow)
```

## Running it

### With Docker (recommended)

```bash
git clone <this repo>
cd whatsapp-mcp
cp .env.example .env   # set INTERNAL_API_SECRET to a random value
docker compose up --build
```

This starts MongoDB, the bridge, and the MCP server. The MCP server listens on `http://localhost:8000/mcp`. The bridge is not published - only the MCP server can reach it, over the compose network.

For a managed MongoDB (e.g. Atlas) instead of the bundled `mongo` service, set `MONGODB_URI` for both `whatsapp-bridge` and `whatsapp-mcp-server` in `docker-compose.yml` and remove the `mongo` service.

Both services need a persistent volume/disk to survive restarts:
- `whatsapp-bridge`'s `/app/store` (whatsmeow's per-device auth store + downloaded media) - already wired up as a named volume in `docker-compose.yml`.
- MongoDB's data directory - same.

When deploying to a platform like **on-demand.io**, translate this into: two containers built from `whatsapp-bridge/Dockerfile` and `whatsapp-mcp-server/Dockerfile`, a MongoDB instance (managed or self-hosted) reachable from both, a persistent volume mounted at `/app/store` on the bridge container, only the MCP server's port exposed publicly, and the environment variables listed below. The exact persistent-volume/env-var mechanics are platform-specific - check on-demand.io's docs for how it expects those to be declared.

### Locally, without Docker

```bash
# terminal 1 - bridge (needs CGO enabled; see Windows note below)
cd whatsapp-bridge
export MONGODB_URI=mongodb://localhost:27017
export INTERNAL_API_SECRET=dev-secret
go run .

# terminal 2 - MCP server
cd whatsapp-mcp-server
export MONGODB_URI=mongodb://localhost:27017
export WHATSAPP_BRIDGE_URL=http://localhost:8080
export INTERNAL_API_SECRET=dev-secret
uv run main.py
```

You'll need a MongoDB instance reachable at `MONGODB_URI` (e.g. `docker run -p 27017:27017 mongo:7`).

### Environment variables

| Variable | Used by | Purpose |
|---|---|---|
| `MONGODB_URI` | both | MongoDB connection string |
| `MONGODB_DATABASE` | both | Database name (default `whatsapp_mcp`) |
| `INTERNAL_API_SECRET` | both | Shared secret authenticating the MCP server to the bridge's internal API - set it to the same random value in both |
| `WHATSAPP_BRIDGE_URL` | mcp server | Base URL of the bridge (e.g. `http://whatsapp-bridge:8080` in Docker) |
| `PORT` | bridge | Port the bridge's internal API listens on (default `8080`) |
| `MCP_HOST` / `MCP_PORT` | mcp server | Bind address for the public streamable-HTTP endpoint (default `0.0.0.0:8000`) |

### Windows Compatibility (bridge)

`go-sqlite3` requires **CGO to be enabled**, and CGO is disabled by default on Windows, so running the bridge outside Docker needs a C compiler:

1. Install a C compiler - we recommend [MSYS2](https://www.msys2.org/) ([setup guide](https://code.visualstudio.com/docs/cpp/config-mingw)), and add its `ucrt64\bin` folder to `PATH`.
2. `go env -w CGO_ENABLED=1` before `go run .`.

(This doesn't apply when running via `docker compose` - the bridge's Dockerfile already builds with a C toolchain.)

## Connecting a WhatsApp account (linking flow)

Each user links their own WhatsApp account from inside their MCP client - no server shell access needed:

1. Point your MCP client at `http://<host>:8000/mcp` (streamable HTTP), with no `Authorization` header yet.
2. Call the `link_whatsapp` tool. It returns `session_id`, `api_token`, and a `qr_png_base64` QR code.
3. Scan the QR code with WhatsApp: **Settings → Linked Devices → Link a Device**.
4. Save `api_token` as your MCP client's `Authorization: Bearer <api_token>` header for this server from now on.
5. Call `get_link_status` to confirm the session is `connected` (it may return a refreshed QR code if the first one expired before you scanned it).

All other tools require that `Authorization` header and only ever operate on the WhatsApp account it's tied to. After roughly 20 days WhatsApp may require re-linking, the same way.

## Usage

### MCP Tools

- **link_whatsapp**: Start linking a new WhatsApp account; returns a QR code and an `api_token` (no auth required - this is the bootstrap step)
- **get_link_status**: Check whether linking has completed for the caller's `api_token`
- **search_contacts**: Search for contacts by name or phone number
- **list_messages**: Retrieve messages with optional filters and context
- **list_chats**: List available chats with metadata
- **get_chat**: Get information about a specific chat
- **get_direct_chat_by_contact**: Find a direct chat with a specific contact
- **get_contact_chats**: List all chats involving a specific contact
- **get_last_interaction**: Get the most recent message with a contact
- **get_message_context**: Retrieve context around a specific message
- **send_message**: Send a WhatsApp message to a specified phone number or group JID
- **send_file**: Send a file (image, video, raw audio, document) to a specified recipient
- **send_audio_message**: Send an audio file as a WhatsApp voice message (requires the file to be an .ogg opus file or ffmpeg must be installed)
- **download_media**: Download media from a WhatsApp message and get the local file path

### Media Handling Features

- **Images, Videos, Documents**: Use the `send_file` tool to share any supported media type.
- **Voice Messages**: Use the `send_audio_message` tool. Audio files should be in `.ogg` Opus format for optimal compatibility - with FFmpeg installed (bundled in the MCP server's Docker image), other formats (MP3, WAV, etc.) are converted automatically. Without FFmpeg, use `send_file` instead (won't appear as a playable voice message).
- **Downloading media**: Only metadata is stored until you call `download_media` with the `message_id` and `chat_jid` shown alongside a media message - it downloads the file and returns a local path.

## Troubleshooting

- **Missing/invalid Authorization header**: every tool except `link_whatsapp` and `get_link_status` needs `Authorization: Bearer <api_token>` from a completed `link_whatsapp` call.
- **"WhatsApp session is not connected yet"**: call `get_link_status` and finish scanning the QR code.
- **QR Code Not Displaying / expired**: QR codes rotate roughly every 60 seconds until scanned; call `get_link_status` again to get a fresh one.
- **Device Limit Reached**: WhatsApp limits linked devices per account; remove an old one under **Settings → Linked Devices** on your phone.
- **No Messages Loading**: after linking, it can take a few minutes for message history to sync, especially with many chats.
- Make sure MongoDB, the bridge, and the MCP server are all reachable from each other (check `MONGODB_URI`, `WHATSAPP_BRIDGE_URL`, and that `INTERNAL_API_SECRET` matches on both services).

For additional MCP client integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues).
