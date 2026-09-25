import dataclasses
import os
from typing import Any, Dict, List, Optional

from mcp.server.fastmcp import Context, FastMCP

import auth
import bridge
import cloud_storage
from whatsapp import (
    search_contacts as whatsapp_search_contacts,
    list_messages as whatsapp_list_messages,
    list_chats as whatsapp_list_chats,
    get_chat as whatsapp_get_chat,
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
    get_contact_chats as whatsapp_get_contact_chats,
    get_last_interaction as whatsapp_get_last_interaction,
    get_message_context as whatsapp_get_message_context,
    send_message as whatsapp_send_message,
    send_file as whatsapp_send_file,
    send_audio_message as whatsapp_audio_voice_message,
    download_media as whatsapp_download_media
)

# Initialize FastMCP server. Every tool below (other than link_whatsapp) requires
# an `Authorization: Bearer <api_token>` header identifying which linked WhatsApp
# account (session) the call should act on - see auth.py.
mcp = FastMCP(
    "whatsapp",
    host=os.environ.get("MCP_HOST", "0.0.0.0"),
    port=int(os.environ.get("MCP_PORT", "8000")),
)


def _asdict(obj: Any) -> Optional[Dict[str, Any]]:
    """Convert a whatsapp.py dataclass (Chat, Contact, ...) into a plain dict for MCP's
    output-schema validation, which rejects raw dataclass instances."""
    if obj is None:
        return None
    return dataclasses.asdict(obj)


def _qr_fields(session_id: str, qr_png_base64: Optional[str]) -> Dict[str, Any]:
    """Prefer a hosted image URL (via Cloudinary) over inlining base64 PNG data.

    Falls back to qr_png_base64 if CLOUDINARY_URL isn't configured or the upload fails.
    """
    if not qr_png_base64:
        return {}
    image_url = cloud_storage.upload_qr(qr_png_base64, session_id)
    if image_url:
        return {"qr_image_url": image_url}
    return {"qr_png_base64": qr_png_base64}


@mcp.tool()
def link_whatsapp() -> Dict[str, Any]:
    """Start linking a new personal WhatsApp account to this server.

    Call this first, with no prior authentication. It returns a QR code image
    to scan with the WhatsApp mobile app, plus an api_token. Save the
    api_token and send it as your MCP client's Authorization header
    (`Bearer <api_token>`) on every subsequent call - it is how this server
    tells your WhatsApp account apart from everyone else's.

    Returns:
        session_id, api_token, and either qr_image_url (a link to the QR code)
        or qr_png_base64 (raw PNG data) if no image hosting is configured.
    """
    created = bridge.create_session()
    session_id = created["session_id"]
    api_token = auth.generate_api_token()
    auth.attach_api_token(session_id, api_token)
    return {
        "session_id": session_id,
        "api_token": api_token,
        **_qr_fields(session_id, created.get("qr_png_base64")),
        "instructions": (
            "Open qr_image_url (or decode qr_png_base64) and scan it with WhatsApp "
            "(Linked Devices > Link a Device), then call get_link_status with this "
            "api_token to confirm the connection."
        ),
    }


@mcp.tool()
def get_link_status(ctx: Context) -> Dict[str, Any]:
    """Check whether the WhatsApp account for the caller's api_token has finished linking.

    If still pending, this may return a refreshed QR code (codes expire after
    about 60 seconds and are rotated automatically until scanned).
    """
    doc = auth.resolve_session_doc(ctx)
    status = bridge.session_status(doc["_id"])
    return {
        "session_id": doc["_id"],
        "status": status.get("status", doc.get("status")),
        **_qr_fields(doc["_id"], status.get("qr_png_base64")),
    }


@mcp.tool()
def search_contacts(ctx: Context, query: str) -> List[Dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number.

    Args:
        query: Search term to match against contact names or phone numbers
    """
    session_id = auth.resolve_session_id(ctx)
    contacts = whatsapp_search_contacts(session_id, query)
    return [_asdict(c) for c in contacts]


@mcp.tool()
def list_messages(
    ctx: Context,
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> str:
    """Get WhatsApp messages matching specified criteria with optional context.

    Args:
        after: Optional ISO-8601 formatted string to only return messages after this date
        before: Optional ISO-8601 formatted string to only return messages before this date
        sender_phone_number: Optional phone number to filter messages by sender
        chat_jid: Optional chat JID to filter messages by chat
        query: Optional search term to filter messages by content
        limit: Maximum number of messages to return (default 20)
        page: Page number for pagination (default 0)
        include_context: Whether to include messages before and after matches (default True)
        context_before: Number of messages to include before each match (default 1)
        context_after: Number of messages to include after each match (default 1)
    """
    session_id = auth.resolve_session_id(ctx)
    return whatsapp_list_messages(
        session_id,
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after
    )


@mcp.tool()
def list_chats(
    ctx: Context,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Dict[str, Any]]:
    """Get WhatsApp chats matching specified criteria.

    Args:
        query: Optional search term to filter chats by name or JID
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
        include_last_message: Whether to include the last message in each chat (default True)
        sort_by: Field to sort results by, either "last_active" or "name" (default "last_active")
    """
    session_id = auth.resolve_session_id(ctx)
    chats = whatsapp_list_chats(
        session_id,
        query=query,
        limit=limit,
        page=page,
        include_last_message=include_last_message,
        sort_by=sort_by
    )
    return [_asdict(c) for c in chats]


@mcp.tool()
def get_chat(ctx: Context, chat_jid: str, include_last_message: bool = True) -> Optional[Dict[str, Any]]:
    """Get WhatsApp chat metadata by JID.

    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)
    """
    session_id = auth.resolve_session_id(ctx)
    return _asdict(whatsapp_get_chat(session_id, chat_jid, include_last_message))


@mcp.tool()
def get_direct_chat_by_contact(ctx: Context, sender_phone_number: str) -> Optional[Dict[str, Any]]:
    """Get WhatsApp chat metadata by sender phone number.

    Args:
        sender_phone_number: The phone number to search for
    """
    session_id = auth.resolve_session_id(ctx)
    return _asdict(whatsapp_get_direct_chat_by_contact(session_id, sender_phone_number))


@mcp.tool()
def get_contact_chats(ctx: Context, jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    """Get all WhatsApp chats involving the contact.

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    session_id = auth.resolve_session_id(ctx)
    chats = whatsapp_get_contact_chats(session_id, jid, limit, page)
    return [_asdict(c) for c in chats]


@mcp.tool()
def get_last_interaction(ctx: Context, jid: str) -> str:
    """Get most recent WhatsApp message involving the contact.

    Args:
        jid: The JID of the contact to search for
    """
    session_id = auth.resolve_session_id(ctx)
    return whatsapp_get_last_interaction(session_id, jid)


@mcp.tool()
def get_message_context(
    ctx: Context,
    message_id: str,
    before: int = 5,
    after: int = 5
) -> Dict[str, Any]:
    """Get context around a specific WhatsApp message.

    Args:
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    session_id = auth.resolve_session_id(ctx)
    return _asdict(whatsapp_get_message_context(session_id, message_id, before, after))


@mcp.tool()
def send_message(ctx: Context, recipient: str, message: str) -> Dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        message: The message text to send

    Returns:
        A dictionary containing success status and a status message
    """
    if not recipient:
        return {"success": False, "message": "Recipient must be provided"}

    session_id = auth.resolve_session_id(ctx)
    success, status_message = whatsapp_send_message(session_id, recipient, message)
    return {"success": success, "message": status_message}


@mcp.tool()
def send_file(ctx: Context, recipient: str, media_url: Optional[str] = None, media_path: Optional[str] = None) -> Dict[str, Any]:
    """Send a file such as a picture, raw audio, video or document via WhatsApp to the specified recipient. For group messages use the JID.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_url: A public URL to the media file to send (image, video, document) - use this,
                 since this server runs remotely and can't see your local filesystem
        media_path: A path on the server's own filesystem (only useful when running this
                 server locally yourself). Provide media_url instead if you have one.

    Returns:
        A dictionary containing success status and a status message
    """
    session_id = auth.resolve_session_id(ctx)
    success, status_message = whatsapp_send_file(session_id, recipient, media_path=media_path, media_url=media_url)
    return {"success": success, "message": status_message}


@mcp.tool()
def send_audio_message(ctx: Context, recipient: str, media_url: Optional[str] = None, media_path: Optional[str] = None) -> Dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_url: A public URL to the audio file to send (will be converted to Opus .ogg if it
                 isn't already) - use this, since this server runs remotely and can't see your
                 local filesystem
        media_path: A path on the server's own filesystem (only useful when running this
                 server locally yourself). Provide media_url instead if you have one.

    Returns:
        A dictionary containing success status and a status message
    """
    session_id = auth.resolve_session_id(ctx)
    success, status_message = whatsapp_audio_voice_message(session_id, recipient, media_path=media_path, media_url=media_url)
    return {"success": success, "message": status_message}


@mcp.tool()
def download_media(ctx: Context, message_id: str, chat_jid: str) -> Dict[str, Any]:
    """Download media from a WhatsApp message and get a link to it.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        A dictionary containing success status, a status message, and (if successful)
        media_url - a public link to the downloaded file.
    """
    session_id = auth.resolve_session_id(ctx)
    file_path = whatsapp_download_media(session_id, message_id, chat_jid)

    if not file_path:
        return {"success": False, "message": "Failed to download media"}

    media_url = cloud_storage.upload_file(file_path, public_id=f"{session_id}/{message_id}")
    result = {"success": True, "message": "Media downloaded successfully"}
    if media_url:
        result["media_url"] = media_url
    else:
        # Cloudinary isn't configured - fall back to the path inside this container,
        # only useful if you have direct access to it (e.g. running locally).
        result["file_path"] = file_path
    return result


if __name__ == "__main__":
    # Streamable HTTP so this one process can serve many users concurrently,
    # each identified by their own bearer token (see auth.py).
    mcp.run(transport='streamable-http')
