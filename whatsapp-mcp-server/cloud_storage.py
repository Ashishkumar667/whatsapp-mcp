"""Cloudinary helpers shared by QR code images and downloaded WhatsApp media.

Both exist for the same reason: this server runs remotely, so neither a raw
base64 blob nor a path inside the container's filesystem is something a
caller can actually use - they need a public URL. Falls back to the raw
data/local path if CLOUDINARY_URL isn't configured, so this stays optional
rather than a hard dependency for local/dev use.

Every upload is also tracked in Mongo's `cloud_uploads` collection so
start_cleanup_loop() can delete it from Cloudinary again after
CLOUDINARY_TTL_SECONDS (default 2 hours) - the same retention policy as
chat/message data (see whatsapp-bridge/mongostore.go), applied to the media
copies this server makes.
"""
import os
import threading
import time
from datetime import datetime, timedelta, timezone
from typing import Optional

import cloudinary
import cloudinary.uploader

import store

DEFAULT_TTL_SECONDS = int(os.environ.get("CLOUDINARY_TTL_SECONDS", str(2 * 60 * 60)))

_configured: Optional[bool] = None


def _ensure_configured() -> bool:
    global _configured
    if _configured is None:
        cloudinary_url = os.environ.get("CLOUDINARY_URL", "")
        if cloudinary_url:
            cloudinary.config(cloudinary_url=cloudinary_url)
        _configured = bool(cloudinary_url)
    return _configured


def _track_upload(result: dict) -> None:
    try:
        store.cloud_uploads().insert_one({
            "public_id": result["public_id"],
            "resource_type": result.get("resource_type", "image"),
            "uploaded_at": datetime.now(timezone.utc),
        })
    except Exception as e:
        print(f"Failed to record cloud upload for TTL tracking: {e}")


def upload_qr(png_base64: str, session_id: str) -> Optional[str]:
    """Upload a QR code PNG (base64) for a session, return its public URL, or None if unconfigured/failed.

    Reuses the same public_id per session so a refreshed QR code (they rotate
    every ~60s until scanned) overwrites the same URL instead of minting a new one.
    """
    if not png_base64 or not _ensure_configured():
        return None
    try:
        result = cloudinary.uploader.upload(
            f"data:image/png;base64,{png_base64}",
            public_id=session_id,
            folder="whatsapp-mcp-qr",
            overwrite=True,
            invalidate=True,
        )
        _track_upload(result)
        return result.get("secure_url")
    except Exception as e:
        print(f"Cloudinary QR upload failed: {e}")
        return None


def upload_file(local_path: str, public_id: str) -> Optional[str]:
    """Upload a downloaded media file (image/video/audio/document) and return its public URL,
    or None if unconfigured/failed."""
    if not local_path or not _ensure_configured():
        return None
    try:
        result = cloudinary.uploader.upload(
            local_path,
            public_id=public_id,
            folder="whatsapp-mcp-media",
            resource_type="auto",
            overwrite=True,
        )
        _track_upload(result)
        return result.get("secure_url")
    except Exception as e:
        print(f"Cloudinary media upload failed: {e}")
        return None


def cleanup_expired(max_age_seconds: int = DEFAULT_TTL_SECONDS) -> int:
    """Delete Cloudinary assets (QR codes and downloaded media) uploaded more than
    max_age_seconds ago, along with their tracking docs. Returns the count removed.
    A no-op if Cloudinary isn't configured."""
    if not _ensure_configured():
        return 0

    cutoff = datetime.now(timezone.utc) - timedelta(seconds=max_age_seconds)
    removed = 0
    for doc in store.cloud_uploads().find({"uploaded_at": {"$lt": cutoff}}):
        try:
            cloudinary.uploader.destroy(
                doc["public_id"], resource_type=doc.get("resource_type", "image"), invalidate=True,
            )
            removed += 1
        except Exception as e:
            print(f"Failed to delete expired Cloudinary asset {doc.get('public_id')}: {e}")
        finally:
            store.cloud_uploads().delete_one({"_id": doc["_id"]})
    return removed


def start_cleanup_loop(interval_seconds: int = 600) -> None:
    """Runs cleanup_expired() on a background daemon thread every interval_seconds.
    Call once at server startup; a no-op (thread just idles) if Cloudinary isn't configured."""
    def _loop():
        while True:
            try:
                removed = cleanup_expired()
                if removed:
                    print(f"Cloudinary TTL cleanup: removed {removed} expired asset(s)")
            except Exception as e:
                print(f"Cloudinary TTL cleanup error: {e}")
            time.sleep(interval_seconds)

    threading.Thread(target=_loop, daemon=True, name="cloudinary-ttl-cleanup").start()
