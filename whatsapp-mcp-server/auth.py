"""Per-user auth: maps a bearer token to the WhatsApp session (tenant) it owns.

Every MCP tool call, other than the bootstrap `link_whatsapp` tool, must be
made with an `Authorization: Bearer <token>` header. The token's hash is
looked up in Mongo's `sessions` collection (the same collection the Go bridge
uses for its own session registry - the bridge owns `jid`/`status`, this
module owns `api_token_hash`) to resolve which session_id the caller may act as.
"""
import hashlib
import secrets
from datetime import datetime, timezone
from typing import Optional

from mcp.server.fastmcp import Context

import store


class AuthError(Exception):
    """Raised when a tool call can't be resolved to a valid, connected session."""


def _hash_token(token: str) -> str:
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


def generate_api_token() -> str:
    return secrets.token_urlsafe(32)


def attach_api_token(session_id: str, api_token: str) -> None:
    """Record the hash of a freshly issued token against a session created by the bridge."""
    store.sessions().update_one(
        {"_id": session_id},
        {"$set": {"api_token_hash": _hash_token(api_token), "updated_at": datetime.now(timezone.utc)}},
    )


def _bearer_token_from_context(ctx: Context) -> Optional[str]:
    request_context = ctx.request_context
    request = getattr(request_context, "request", None)
    if request is None:
        # stdio / non-HTTP transport - no per-request auth is possible.
        return None
    auth_header = request.headers.get("authorization", "")
    if not auth_header.lower().startswith("bearer "):
        return None
    return auth_header[len("bearer "):].strip()


def resolve_session_doc(ctx: Context) -> dict:
    """Return the session document the caller's bearer token is authorized for, or raise AuthError."""
    token = _bearer_token_from_context(ctx)
    if not token:
        raise AuthError("Missing bearer token. Call link_whatsapp first, then pass its api_token as your Authorization header.")

    doc = store.sessions().find_one({"api_token_hash": _hash_token(token)})
    if not doc:
        raise AuthError("Unknown or revoked API token.")
    return doc


def resolve_session_id(ctx: Context) -> str:
    """Return the session_id for a caller whose WhatsApp session is fully connected."""
    doc = resolve_session_doc(ctx)
    if doc.get("status") != "connected":
        raise AuthError(f"WhatsApp session is not connected yet (status: {doc.get('status')}). Use get_link_status to check progress.")
    return doc["_id"]
