"""Shared MongoDB access.

The Go bridge (whatsapp-bridge) owns writes to the `sessions`, `chats`, and
`messages` collections. This module gives the Python MCP server read access
to `chats`/`messages` (for the read-oriented tools) and both read/write access
to `sessions` (to mint API tokens for newly linked WhatsApp accounts).
"""
import os
from datetime import datetime
from typing import Optional

from pymongo import MongoClient
from pymongo.collection import Collection

MONGODB_URI = os.environ.get("MONGODB_URI", "mongodb://localhost:27017")
MONGODB_DATABASE = os.environ.get("MONGODB_DATABASE", "whatsapp_mcp")

_client: Optional[MongoClient] = None


def get_client() -> MongoClient:
    global _client
    if _client is None:
        _client = MongoClient(MONGODB_URI)
    return _client


def _db():
    return get_client()[MONGODB_DATABASE]


def sessions() -> Collection:
    return _db()["sessions"]


def chats() -> Collection:
    return _db()["chats"]


def messages() -> Collection:
    return _db()["messages"]


def to_datetime(value) -> Optional[datetime]:
    """Normalize whatever pymongo/bson handed back into a plain datetime, or None."""
    if value is None:
        return None
    if isinstance(value, datetime):
        return value
    return datetime.fromisoformat(str(value))
