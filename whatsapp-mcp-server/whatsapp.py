import os
import os.path
import re
from dataclasses import dataclass
from datetime import datetime
from typing import List, Optional, Tuple

import audio
import bridge
import media_fetch
import store

WHATSAPP_MEDIA_ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge')


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None


@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")


@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str


@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]


def _chat_name(session_id: str, chat_jid: str, cache: Optional[dict] = None) -> Optional[str]:
    if cache is not None and chat_jid in cache:
        return cache[chat_jid]
    doc = store.chats().find_one({"session_id": session_id, "jid": chat_jid}, {"name": 1})
    name = doc.get("name") if doc else None
    if cache is not None:
        cache[chat_jid] = name
    return name


def _message_from_doc(doc: dict, chat_name: Optional[str] = None) -> Message:
    return Message(
        timestamp=store.to_datetime(doc["timestamp"]),
        sender=doc.get("sender", ""),
        content=doc.get("content", ""),
        is_from_me=bool(doc.get("is_from_me", False)),
        chat_jid=doc["chat_jid"],
        id=doc["id"],
        chat_name=chat_name,
        media_type=doc.get("media_type") or None,
    )


def get_sender_name(session_id: str, sender_jid: str) -> str:
    doc = store.chats().find_one({"session_id": session_id, "jid": sender_jid})
    if not doc:
        phone_part = sender_jid.split('@')[0] if '@' in sender_jid else sender_jid
        doc = store.chats().find_one({
            "session_id": session_id,
            "jid": {"$regex": re.escape(phone_part)},
        })
    if doc and doc.get("name"):
        return doc["name"]
    return sender_jid


def format_message(session_id: str, message: Message, show_chat_info: bool = True) -> str:
    """Format a single message with consistent formatting."""
    output = ""

    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "

    content_prefix = ""
    if message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "

    try:
        sender_name = get_sender_name(session_id, message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        print(f"Error formatting message: {e}")
    return output


def format_messages_list(session_id: str, messages: List[Message], show_chat_info: bool = True) -> str:
    if not messages:
        return "No messages to display."

    output = ""
    for message in messages:
        output += format_message(session_id, message, show_chat_info)
    return output


def list_messages(
    session_id: str,
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
    """Get messages matching the specified criteria with optional context."""
    match: dict = {"session_id": session_id}

    if after:
        try:
            after_dt = datetime.fromisoformat(after)
        except ValueError:
            raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
        match.setdefault("timestamp", {})["$gt"] = after_dt

    if before:
        try:
            before_dt = datetime.fromisoformat(before)
        except ValueError:
            raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
        match.setdefault("timestamp", {})["$lt"] = before_dt

    if sender_phone_number:
        match["sender"] = sender_phone_number
    if chat_jid:
        match["chat_jid"] = chat_jid
    if query:
        match["content"] = {"$regex": re.escape(query), "$options": "i"}

    cursor = (
        store.messages()
        .find(match)
        .sort("timestamp", -1)
        .skip(page * limit)
        .limit(limit)
    )

    name_cache: dict = {}
    result = []
    for doc in cursor:
        name = _chat_name(session_id, doc["chat_jid"], name_cache)
        result.append(_message_from_doc(doc, name))

    if include_context and result:
        messages_with_context = []
        for msg in result:
            context = get_message_context(session_id, msg.id, context_before, context_after)
            messages_with_context.extend(context.before)
            messages_with_context.append(context.message)
            messages_with_context.extend(context.after)
        return format_messages_list(session_id, messages_with_context, show_chat_info=True)

    return format_messages_list(session_id, result, show_chat_info=True)


def get_message_context(
    session_id: str,
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific message."""
    msg_doc = store.messages().find_one({"session_id": session_id, "id": message_id})
    if not msg_doc:
        raise ValueError(f"Message with ID {message_id} not found")

    name_cache: dict = {}
    chat_jid = msg_doc["chat_jid"]
    name = _chat_name(session_id, chat_jid, name_cache)
    target_message = _message_from_doc(msg_doc, name)

    before_cursor = (
        store.messages()
        .find({"session_id": session_id, "chat_jid": chat_jid, "timestamp": {"$lt": msg_doc["timestamp"]}})
        .sort("timestamp", -1)
        .limit(before)
    )
    before_messages = [_message_from_doc(d, name) for d in before_cursor]

    after_cursor = (
        store.messages()
        .find({"session_id": session_id, "chat_jid": chat_jid, "timestamp": {"$gt": msg_doc["timestamp"]}})
        .sort("timestamp", 1)
        .limit(after)
    )
    after_messages = [_message_from_doc(d, name) for d in after_cursor]

    return MessageContext(message=target_message, before=before_messages, after=after_messages)


def _chat_from_doc(doc: dict, last_message_doc: Optional[dict] = None) -> Chat:
    return Chat(
        jid=doc["jid"],
        name=doc.get("name"),
        last_message_time=store.to_datetime(doc.get("last_message_time")),
        last_message=(last_message_doc or {}).get("content"),
        last_sender=(last_message_doc or {}).get("sender"),
        last_is_from_me=(last_message_doc or {}).get("is_from_me"),
    )


def _last_message_for_chat(session_id: str, chat_jid: str, last_message_time) -> Optional[dict]:
    if last_message_time is None:
        return None
    return store.messages().find_one({
        "session_id": session_id, "chat_jid": chat_jid, "timestamp": last_message_time,
    })


def list_chats(
    session_id: str,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    match: dict = {"session_id": session_id}
    if query:
        match["$or"] = [
            {"name": {"$regex": re.escape(query), "$options": "i"}},
            {"jid": {"$regex": re.escape(query), "$options": "i"}},
        ]

    sort_field = "last_message_time" if sort_by == "last_active" else "name"
    sort_dir = -1 if sort_by == "last_active" else 1

    cursor = (
        store.chats()
        .find(match)
        .sort(sort_field, sort_dir)
        .skip(page * limit)
        .limit(limit)
    )

    result = []
    for doc in cursor:
        last_message_doc = _last_message_for_chat(session_id, doc["jid"], doc.get("last_message_time")) if include_last_message else None
        result.append(_chat_from_doc(doc, last_message_doc))
    return result


def search_contacts(session_id: str, query: str) -> List[Contact]:
    """Search contacts by name or phone number."""
    pattern = re.escape(query)
    cursor = store.chats().find({
        "session_id": session_id,
        "jid": {"$not": re.compile(r"@g\.us$")},
        "$or": [
            {"name": {"$regex": pattern, "$options": "i"}},
            {"jid": {"$regex": pattern, "$options": "i"}},
        ],
    }).sort([("name", 1), ("jid", 1)]).limit(50)

    result = []
    for doc in cursor:
        result.append(Contact(
            phone_number=doc["jid"].split('@')[0],
            name=doc.get("name"),
            jid=doc["jid"],
        ))
    return result


def get_contact_chats(session_id: str, jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact."""
    message_chat_jids = store.messages().distinct("chat_jid", {"session_id": session_id, "sender": jid})
    chat_jids = set(message_chat_jids) | {jid}

    cursor = (
        store.chats()
        .find({"session_id": session_id, "jid": {"$in": list(chat_jids)}})
        .sort("last_message_time", -1)
        .skip(page * limit)
        .limit(limit)
    )

    result = []
    for doc in cursor:
        last_message_doc = _last_message_for_chat(session_id, doc["jid"], doc.get("last_message_time"))
        result.append(_chat_from_doc(doc, last_message_doc))
    return result


def get_last_interaction(session_id: str, jid: str) -> Optional[str]:
    """Get most recent message involving the contact."""
    doc = store.messages().find_one(
        {"session_id": session_id, "$or": [{"sender": jid}, {"chat_jid": jid}]},
        sort=[("timestamp", -1)],
    )
    if not doc:
        return None

    name = _chat_name(session_id, doc["chat_jid"])
    return format_message(session_id, _message_from_doc(doc, name))


def get_chat(session_id: str, chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    doc = store.chats().find_one({"session_id": session_id, "jid": chat_jid})
    if not doc:
        return None
    last_message_doc = _last_message_for_chat(session_id, chat_jid, doc.get("last_message_time")) if include_last_message else None
    return _chat_from_doc(doc, last_message_doc)


def get_direct_chat_by_contact(session_id: str, sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number."""
    doc = store.chats().find_one({
        "session_id": session_id,
        "$and": [
            {"jid": {"$regex": re.escape(sender_phone_number)}},
            {"jid": {"$not": re.compile(r"@g\.us$")}},
        ],
    })
    if not doc:
        return None
    last_message_doc = _last_message_for_chat(session_id, doc["jid"], doc.get("last_message_time"))
    return _chat_from_doc(doc, last_message_doc)


def send_message(session_id: str, recipient: str, message: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    try:
        return bridge.send(session_id, recipient, message=message)
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def _resolve_media_path(media_path: Optional[str], media_url: Optional[str]) -> Tuple[Optional[str], Optional[str]]:
    """Returns (path_to_send, temp_path_to_clean_up_afterward)."""
    if media_url:
        temp_path = media_fetch.download_to_temp(media_url)
        return temp_path, temp_path
    return media_path, None


def send_file(session_id: str, recipient: str, media_path: Optional[str] = None, media_url: Optional[str] = None) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    if not media_path and not media_url:
        return False, "Either media_path or media_url must be provided"

    temp_path = None
    try:
        media_path, temp_path = _resolve_media_path(media_path, media_url)
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"
        return bridge.send(session_id, recipient, media_path=media_path)
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"
    finally:
        if temp_path and os.path.exists(temp_path):
            os.remove(temp_path)


def send_audio_message(session_id: str, recipient: str, media_path: Optional[str] = None, media_url: Optional[str] = None) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    if not media_path and not media_url:
        return False, "Either media_path or media_url must be provided"

    temp_path = None
    try:
        media_path, temp_path = _resolve_media_path(media_path, media_url)
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        if not media_path.endswith(".ogg"):
            try:
                converted_path = audio.convert_to_opus_ogg_temp(media_path)
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"
            if temp_path and os.path.exists(temp_path):
                os.remove(temp_path)
            media_path = temp_path = converted_path

        return bridge.send(session_id, recipient, media_path=media_path)
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"
    finally:
        if temp_path and os.path.exists(temp_path):
            os.remove(temp_path)


def download_media(session_id: str, message_id: str, chat_jid: str) -> Optional[str]:
    """Download media from a message and return the local file path."""
    try:
        return bridge.download(session_id, message_id, chat_jid)
    except Exception as e:
        print(f"Unexpected error downloading media: {str(e)}")
        return None
