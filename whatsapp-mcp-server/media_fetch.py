"""Downloads a public URL (image/video/audio/document) to a local temp file so it
can be handed to the bridge's send flow, which only ever takes a local path.

The temp file's extension matters: the bridge picks the WhatsApp media type
(image/video/audio/document) from the file's extension, so this makes a best
effort to give it a real one based on the URL or the response's Content-Type.
"""
import mimetypes
import os
import tempfile
from typing import Optional
from urllib.parse import urlparse

import requests

_KNOWN_EXTENSIONS = {".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp4", ".avi", ".mov", ".ogg"}

_EXT_BY_CONTENT_TYPE = {
    "image/jpeg": ".jpg",
    "image/png": ".png",
    "image/gif": ".gif",
    "image/webp": ".webp",
    "video/mp4": ".mp4",
    "video/quicktime": ".mov",
    "video/x-msvideo": ".avi",
    "audio/ogg": ".ogg",
}


def _guess_extension(url: str, content_type: Optional[str]) -> str:
    ext = os.path.splitext(urlparse(url).path)[1].lower()
    if ext in _KNOWN_EXTENSIONS:
        return ext

    content_type = (content_type or "").split(";")[0].strip().lower()
    return _EXT_BY_CONTENT_TYPE.get(content_type) or mimetypes.guess_extension(content_type) or ""


def download_to_temp(url: str) -> str:
    """Download url to a new temp file and return its local path. Caller is responsible
    for deleting it once done (e.g. after sending it on)."""
    resp = requests.get(url, timeout=60, stream=True)
    resp.raise_for_status()

    ext = _guess_extension(url, resp.headers.get("Content-Type"))
    fd, path = tempfile.mkstemp(suffix=ext)
    try:
        with os.fdopen(fd, "wb") as f:
            for chunk in resp.iter_content(chunk_size=65536):
                if chunk:
                    f.write(chunk)
    except Exception:
        os.remove(path)
        raise
    return path
