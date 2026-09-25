"""HTTP client for the Go bridge's internal REST API.

The bridge is not reachable from outside the deployment - only this server
talks to it, authenticating with a shared secret. Per-user identity has
already been resolved (see auth.py) by the time these functions are called;
they operate on a specific session_id.
"""
import os
from typing import Optional, Tuple

import requests

WHATSAPP_BRIDGE_URL = os.environ.get("WHATSAPP_BRIDGE_URL", "http://localhost:8080")
INTERNAL_API_SECRET = os.environ.get("INTERNAL_API_SECRET", "")


def _headers() -> dict:
    if not INTERNAL_API_SECRET:
        raise RuntimeError("INTERNAL_API_SECRET is not configured")
    return {"X-Internal-Secret": INTERNAL_API_SECRET}


def create_session() -> dict:
    """Start pairing a brand-new WhatsApp device. Returns {session_id, qr_png_base64}."""
    resp = requests.post(f"{WHATSAPP_BRIDGE_URL}/internal/sessions", headers=_headers(), timeout=35)
    resp.raise_for_status()
    return resp.json()


def session_status(session_id: str) -> dict:
    """Returns {status, qr_png_base64?}."""
    resp = requests.get(f"{WHATSAPP_BRIDGE_URL}/internal/sessions/{session_id}/status", headers=_headers(), timeout=10)
    resp.raise_for_status()
    return resp.json()


def send(session_id: str, recipient: str, message: str = "", media_path: str = "") -> Tuple[bool, str]:
    payload = {"recipient": recipient, "message": message}
    if media_path:
        payload["media_path"] = media_path
    resp = requests.post(
        f"{WHATSAPP_BRIDGE_URL}/internal/sessions/{session_id}/send",
        headers=_headers(), json=payload, timeout=60,
    )
    try:
        result = resp.json()
    except ValueError:
        return False, f"Error: HTTP {resp.status_code} - {resp.text}"
    return result.get("success", False), result.get("message", "Unknown response")


def download(session_id: str, message_id: str, chat_jid: str) -> Tuple[Optional[str], str]:
    """Returns (local_path, message). local_path is None on failure - message explains why."""
    resp = requests.post(
        f"{WHATSAPP_BRIDGE_URL}/internal/sessions/{session_id}/download",
        headers=_headers(), json={"message_id": message_id, "chat_jid": chat_jid}, timeout=60,
    )
    try:
        result = resp.json()
    except ValueError:
        return None, f"Error: HTTP {resp.status_code} - {resp.text}"
    if resp.status_code == 200 and result.get("success"):
        return result.get("path"), result.get("message", "")
    return None, result.get("message") or result.get("error") or f"HTTP {resp.status_code}"
