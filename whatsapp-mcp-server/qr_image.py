"""Uploads the bridge's base64 QR code PNG to Cloudinary and hands back a URL.

Falls back to returning the raw base64 PNG if CLOUDINARY_URL isn't configured,
so this stays optional rather than a hard dependency for local/dev use.
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
