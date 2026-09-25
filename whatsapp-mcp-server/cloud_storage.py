"""Cloudinary helpers shared by QR code images and downloaded WhatsApp media.

Both exist for the same reason: this server runs remotely, so neither a raw
base64 blob nor a path inside the container's filesystem is something a
caller can actually use - they need a public URL. Falls back to the raw
data/local path if CLOUDINARY_URL isn't configured, so this stays optional
rather than a hard dependency for local/dev use.
"""
import os
from typing import Optional

import cloudinary
import cloudinary.uploader

_configured: Optional[bool] = None


def _ensure_configured() -> bool:
    global _configured
    if _configured is None:
        cloudinary_url = os.environ.get("CLOUDINARY_URL", "")
        if cloudinary_url:
            cloudinary.config(cloudinary_url=cloudinary_url)
        _configured = bool(cloudinary_url)
    return _configured


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
        return result.get("secure_url")
    except Exception as e:
        print(f"Cloudinary media upload failed: {e}")
        return None
