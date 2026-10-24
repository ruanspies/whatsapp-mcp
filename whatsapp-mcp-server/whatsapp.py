import hashlib
import json
import logging
import os
import os.path
import pathlib
import re
import sqlite3
from contextlib import contextmanager
from datetime import datetime, timezone
from typing import Any, Dict, Iterator, List, Optional, Tuple

import requests
from mcp.server.mcpserver.exceptions import ToolError

import audio

logger = logging.getLogger("whatsapp-mcp")

DEFAULT_STORE_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store')

MAX_MESSAGE_LIMIT = 500
MAX_CHAT_LIMIT = 200
MAX_CONTEXT = 100
PREVIEW_LENGTH = 300
SEND_TIMEOUT = 30
SEND_MEDIA_TIMEOUT = 180  # includes the upload
DOWNLOAD_TIMEOUT = 180

USER_SERVER = "@s.whatsapp.net"
GROUP_SERVER = "@g.us"
LID_SERVER = "@lid"

# Timestamps are stored as UTC text in exactly this format, so they compare lexically.
DB_TIME_FORMAT = "%Y-%m-%d %H:%M:%S+00:00"

MESSAGE_COLUMNS = """
    m.rowid AS rowid, m.id, m.chat_jid, m.sender, m.content, m.timestamp,
    m.is_from_me, m.media_type, m.filename, c.name AS chat_name
"""


class WhatsAppError(ToolError):
    """An anticipated failure. As a ToolError its message reaches the tool caller
    (any other exception is reported by the MCP server only as a generic crash)."""


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

def store_dir() -> str:
    return os.environ.get("WHATSAPP_MCP_STORE_DIR") or DEFAULT_STORE_DIR

def messages_db_path() -> str:
    return os.path.join(store_dir(), "messages.db")

def contacts_db_path() -> str:
    return os.path.join(store_dir(), "whatsapp.db")

def api_token_path() -> str:
    return os.path.join(store_dir(), "api_token")

def api_base_url() -> str:
    port = os.environ.get("WHATSAPP_MCP_PORT") or "8080"
    return f"http://127.0.0.1:{port}/api"


# ---------------------------------------------------------------------------
# Database access (read-only)
# ---------------------------------------------------------------------------

def _connect_readonly(path: str) -> sqlite3.Connection:
    uri = pathlib.Path(path).resolve().as_uri() + "?mode=ro"
    conn = sqlite3.connect(uri, uri=True, timeout=5)
    conn.row_factory = sqlite3.Row
    # SQLite's own LIKE and LOWER only fold ASCII case
    conn.create_function("casefold", 1, lambda text: text.casefold() if isinstance(text, str) else text, deterministic=True)
    return conn

@contextmanager
def _messages_db() -> Iterator[sqlite3.Connection]:
    """Open messages.db read-only; turn database failures into a WhatsAppError."""
    path = messages_db_path()
    if not os.path.isfile(path):
        raise WhatsAppError(
            f"WhatsApp message database not found at {path}. "
            "The WhatsApp bridge has not been run and paired yet: start whatsapp-bridge and scan the QR code."
        )
    conn = None
    try:
        conn = _connect_readonly(path)
        yield conn
    except sqlite3.Error as e:
        logger.exception("Database error")
        raise WhatsAppError(f"Database error reading {path}: {e}") from e
    finally:
        if conn is not None:
            conn.close()

def _like(term: str) -> str:
    """Build a substring LIKE pattern (use with ESCAPE '\\')."""
    escaped = term.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
    return f"%{escaped}%"


# ---------------------------------------------------------------------------
# Time handling
# ---------------------------------------------------------------------------

def _to_db_time(value: str, field: str) -> str:
    """Parse an ISO-8601 string (naive = local time) into the stored UTC text format."""
    try:
        parsed = datetime.fromisoformat(value.strip())
    except (ValueError, AttributeError):
        raise WhatsAppError(
            f"Invalid date format for '{field}': {value!r}. Use ISO-8601, e.g. 2026-01-31T18:00 "
            "(local time) or 2026-01-31T18:00:00+02:00."
        )
    # astimezone() treats a naive datetime as local time.
    return parsed.astimezone(timezone.utc).strftime(DB_TIME_FORMAT)

def _to_local(stored: Optional[str]) -> Optional[datetime]:
    """Parse a stored timestamp into an aware local datetime (None if unparseable)."""
    if not stored:
        return None
    try:
        parsed = datetime.fromisoformat(stored)
    except (ValueError, TypeError):
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone()

def _local_iso(stored: Optional[str]) -> Optional[str]:
    local = _to_local(stored)
    return local.isoformat(timespec="seconds") if local else stored


# ---------------------------------------------------------------------------
# JIDs, phone numbers and names
# ---------------------------------------------------------------------------

def _digits(value: str) -> str:
    """Bare phone digits from '+27 82 123 4567', '27821234567@s.whatsapp.net', '2782...:12@...'."""
    user = value.split("@")[0].split(":")[0]
    return "".join(ch for ch in user if ch.isdigit())

def _normalize_sender(value: str) -> str:
    """Normalise a sender filter to how messages.sender is stored."""
    value = value.strip()
    if value.endswith(LID_SERVER):
        return value
    digits = _digits(value)
    if not digits:
        raise WhatsAppError(
            f"Invalid phone number: {value!r}. Use digits with country code (e.g. 27821234567) or a JID."
        )
    return digits

def _contact_keys(value: str) -> Tuple[Optional[str], str]:
    """Resolve a contact reference (JID or phone number) to (messages.sender value, direct chat JID)."""
    value = value.strip()
    if value.endswith(GROUP_SERVER):
        return None, value
    if value.endswith(LID_SERVER):
        return value, value
    digits = _normalize_sender(value)
    return digits, digits + USER_SERVER

def _load_contacts() -> Dict[str, str]:
    """Map JID -> best display name from whatsmeow's own store. Empty if unavailable."""
    contacts: Dict[str, str] = {}
    path = contacts_db_path()
    if not os.path.isfile(path):
        return contacts
    conn = None
    try:
        conn = _connect_readonly(path)
        for row in conn.execute("SELECT * FROM whatsmeow_contacts"):
            keys = row.keys()
            jid = row["their_jid"] if "their_jid" in keys else None
            if not jid or jid in contacts:
                continue
            for column in ("full_name", "first_name", "push_name", "business_name"):
                name = row[column] if column in keys else None
                if name and str(name).strip():
                    contacts[jid] = str(name).strip()
                    break
    except Exception as e:  # schema of whatsapp.db is not ours: never fail on it
        logger.warning("Could not read contact names from %s: %s", path, e)
    finally:
        if conn is not None:
            conn.close()
    return contacts

class _Names:
    """Exact-match name resolution, cached for the duration of one tool call."""

    def __init__(self, conn: sqlite3.Connection):
        self._conn = conn
        self._contacts: Optional[Dict[str, str]] = None
        self._chat_names: Dict[str, Optional[str]] = {}
        self._senders: Dict[str, str] = {}

    @property
    def conn(self) -> sqlite3.Connection:
        return self._conn

    @property
    def contacts(self) -> Dict[str, str]:
        if self._contacts is None:
            self._contacts = _load_contacts()
        return self._contacts

    def _chat_name(self, jid: str) -> Optional[str]:
        if jid not in self._chat_names:
            row = self._conn.execute("SELECT name FROM chats WHERE jid = ?", (jid,)).fetchone()
            self._chat_names[jid] = row["name"] if row else None
        return self._chat_names[jid]

    def _person(self, jid: str, chat_name: Optional[str]) -> str:
        user = jid.split("@")[0]
        name = self.contacts.get(jid)
        if name:
            return _display_name(name)
        # chats.name falls back to the number, which is not a real name.
        if chat_name and chat_name.strip() and chat_name != user and chat_name != jid:
            return _display_name(chat_name)
        if jid.endswith(LID_SERVER):
            return f"unknown ({jid})"  # a hidden ID, not a phone number
        return user

    def sender(self, sender: Optional[str]) -> str:
        """Display name for a messages.sender value (bare digits or '<digits>@lid')."""
        if not sender:
            return "unknown"
        if sender not in self._senders:
            jid = sender if "@" in sender else sender + USER_SERVER
            self._senders[sender] = self._person(jid, self._chat_name(jid))
        return self._senders[sender]

    def chat(self, jid: str, chat_name: Optional[str]) -> str:
        """Display name for a chat."""
        if jid.endswith(GROUP_SERVER):
            return _display_name(chat_name) if chat_name else jid
        return self._person(jid, chat_name)


# ---------------------------------------------------------------------------
# Formatting
# ---------------------------------------------------------------------------

# Everything below is written by other people (message text, file names, the names
# they give themselves and their groups, even message IDs), so none of it may be
# able to imitate the structure of a transcript.

def _one_line(text: Any) -> str:
    """Collapse line breaks and other whitespace runs to single spaces."""
    return " ".join(str(text).split())

def _display_name(name: str) -> str:
    name = _one_line(name)
    # "Me" is how the account owner is shown; nobody else may appear under it
    if name.casefold() in ("me", "me:"):
        return f'"{name}" (a contact, not you)'
    return name

def _display_id(message_id: Any) -> str:
    message_id = str(message_id)
    return message_id if re.fullmatch(r"[A-Za-z0-9_.:@-]+", message_id) else json.dumps(message_id)

def _media_marker(row: sqlite3.Row) -> str:
    if not row["media_type"]:
        return ""
    media_type = _one_line(row["media_type"]).replace("]", ")")
    if row["filename"]:
        return f"[{media_type}: {_one_line(row['filename']).replace(']', ')')}] "
    return f"[{media_type}] "

def _message_text(row: sqlite3.Row) -> str:
    """Media marker plus text. Further lines of a message are indented, so only a
    real message can start at the beginning of a line."""
    content = "\n    ".join((row["content"] or "").splitlines())
    return (_media_marker(row) + content).strip()

def _preview_text(row: sqlite3.Row) -> str:
    return (_media_marker(row) + _one_line(row["content"] or "")).strip()

def _format_transcript(
    names: _Names,
    chats: List[Tuple[str, List[Tuple[Optional[int], sqlite3.Row]]]],
    marked: Optional[set] = None,
) -> str:
    """Render messages as a transcript grouped by chat, oldest to newest.

    chats: [(chat_jid, [(position_in_chat or None, row), ...])] with rows already ascending.
    marked: (id, chat_jid) keys to prefix with '> ' (search hits / the context target).
    A '...' line marks a gap where positions are known and not consecutive.
    """
    polls = _load_polls(names.conn, [(row["id"], row["chat_jid"]) for _, entries in chats for _, row in entries])
    lines: List[str] = []
    for chat_jid, entries in chats:
        if lines:
            lines.append("")
        chat_name = entries[0][1]["chat_name"]
        kind = "group" if chat_jid.endswith(GROUP_SERVER) else "direct"
        lines.append(f"Chat: {names.chat(chat_jid, chat_name)} ({chat_jid}, {kind})")
        current_day = None
        previous_position = None
        for position, row in entries:
            local = _to_local(row["timestamp"])
            if local is not None:
                offset = local.strftime("%z")
                day = f"-- {local:%a %Y-%m-%d} UTC{offset[:3]}:{offset[3:]} --"
                clock = f"{local:%H:%M}"
            else:
                day = "-- unknown date --"
                clock = str(row["timestamp"])
            if previous_position is not None and position is not None and position - previous_position > 1:
                lines.append("...")
            if day != current_day:
                lines.append(day)
                current_day = day
            previous_position = position
            who = "Me" if row["is_from_me"] else names.sender(row["sender"])
            mark = "> " if marked and (row["id"], row["chat_jid"]) in marked else ""
            poll = polls.get((row["id"], row["chat_jid"]))
            if poll:
                lines.append(f"{mark}[{clock} id={_display_id(row['id'])}] {who}: {_poll_lines(poll)}")
            else:
                lines.append(f"{mark}[{clock} id={_display_id(row['id'])}] {who}: {_message_text(row)}")
    return "\n".join(lines)

def _chat_positions(conn: sqlite3.Connection, chat_jid: str) -> List[int]:
    """All message rowids of a chat in chronological order."""
    cursor = conn.execute(
        "SELECT rowid FROM messages WHERE chat_jid = ? ORDER BY timestamp ASC, rowid ASC", (chat_jid,)
    )
    return [r[0] for r in cursor]

def _fetch_rows(conn: sqlite3.Connection, rowids: List[int]) -> Dict[int, sqlite3.Row]:
    rows: Dict[int, sqlite3.Row] = {}
    for start in range(0, len(rowids), 500):
        chunk = rowids[start:start + 500]
        placeholders = ",".join("?" * len(chunk))
        cursor = conn.execute(
            f"SELECT {MESSAGE_COLUMNS} FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid "
            f"WHERE m.rowid IN ({placeholders})",
            chunk,
        )
        for row in cursor:
            rows[row["rowid"]] = row
    return rows

def _expand_context(
    conn: sqlite3.Connection, chat_jid: str, hit_rowids: List[int], before: int, after: int
) -> List[Tuple[Optional[int], sqlite3.Row]]:
    """Hits of one chat plus surrounding messages: de-duplicated, chronological, with positions."""
    order = _chat_positions(conn, chat_jid)
    index = {rowid: i for i, rowid in enumerate(order)}
    wanted = set()
    for rowid in hit_rowids:
        if rowid in index:
            i = index[rowid]
            wanted.update(range(max(0, i - before), min(len(order), i + after + 1)))
    positions = sorted(wanted)
    rows = _fetch_rows(conn, [order[i] for i in positions])
    return [(i, rows[order[i]]) for i in positions if order[i] in rows]

def _group_by_chat(rows: List[sqlite3.Row]) -> List[Tuple[str, List[sqlite3.Row]]]:
    """Group ascending rows per chat; chats ordered by their newest message, most recent last."""
    grouped: Dict[str, List[sqlite3.Row]] = {}
    for row in rows:
        grouped.setdefault(row["chat_jid"], []).append(row)
    return sorted(grouped.items(), key=lambda item: (item[1][-1]["timestamp"], item[1][-1]["rowid"]))

def _clamp(value: int, low: int, high: int) -> int:
    return max(low, min(int(value), high))


# ---------------------------------------------------------------------------
# Messages
# ---------------------------------------------------------------------------

def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 50,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> str:
    """Get the newest messages matching the criteria as a chronological transcript."""
    limit = _clamp(limit, 1, MAX_MESSAGE_LIMIT)
    page = max(0, int(page))

    where_clauses = []
    params: List[Any] = []

    if after:
        where_clauses.append("m.timestamp >= ?")
        params.append(_to_db_time(after, "after"))
    if before:
        where_clauses.append("m.timestamp < ?")
        params.append(_to_db_time(before, "before"))
    if sender_phone_number:
        where_clauses.append("m.sender = ?")
        params.append(_normalize_sender(sender_phone_number))
    if chat_jid:
        where_clauses.append("m.chat_jid = ?")
        params.append(chat_jid.strip())
    if query:
        where_clauses.append("instr(casefold(m.content), ?) > 0")
        params.append(query.casefold())

    sql = f"SELECT {MESSAGE_COLUMNS} FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid"
    if where_clauses:
        sql += " WHERE " + " AND ".join(where_clauses)
    # Newest `limit` rows (one extra to know whether older ones exist), shown oldest first.
    sql += " ORDER BY m.timestamp DESC, m.rowid DESC LIMIT ? OFFSET ?"
    params.extend([limit + 1, page * limit])

    with _messages_db() as conn:
        rows = conn.execute(sql, params).fetchall()
        has_older = len(rows) > limit
        rows = rows[:limit]
        if not rows:
            return "No messages found." if page == 0 else "No more messages."
        rows.reverse()

        names = _Names(conn)
        with_context = bool(query) and include_context
        context_before = _clamp(context_before, 0, MAX_CONTEXT)
        context_after = _clamp(context_after, 0, MAX_CONTEXT)
        marked = None
        chats = []
        for jid, chat_rows in _group_by_chat(rows):
            if with_context and (context_before or context_after):
                entries = _expand_context(
                    conn, jid, [r["rowid"] for r in chat_rows], context_before, context_after
                )
                marked = marked or set()
                marked.update((r["id"], r["chat_jid"]) for r in chat_rows)
            else:
                entries = [(None, r) for r in chat_rows]
            chats.append((jid, entries))

        output = _format_transcript(names, chats, marked)
        if has_older:
            output = (
                f"(Showing the newest {len(rows)} matching messages; older ones exist. "
                f"Use page={page + 1} or narrow with after/before.)\n" + output
            )
        return output


def get_message_context(
    message_id: str,
    chat_jid: Optional[str] = None,
    before: int = 5,
    after: int = 5
) -> str:
    """Get a transcript of the messages around a specific message, keyed on (message_id, chat_jid)."""
    before = _clamp(before, 0, MAX_CONTEXT)
    after = _clamp(after, 0, MAX_CONTEXT)

    with _messages_db() as conn:
        sql = "SELECT rowid, id, chat_jid FROM messages WHERE id = ?"
        params = [message_id]
        if chat_jid:
            sql += " AND chat_jid = ?"
            params.append(chat_jid.strip())
        targets = conn.execute(sql, params).fetchall()

        if not targets:
            where = f" in chat {chat_jid}" if chat_jid else ""
            raise WhatsAppError(f"Message with ID {message_id} not found{where}.")
        if len(targets) > 1:
            chats = ", ".join(t["chat_jid"] for t in targets)
            raise WhatsAppError(
                f"Message ID {message_id} exists in several chats ({chats}). Pass chat_jid to choose one."
            )

        target = targets[0]
        entries = _expand_context(conn, target["chat_jid"], [target["rowid"]], before, after)
        return _format_transcript(
            _Names(conn), [(target["chat_jid"], entries)], {(target["id"], target["chat_jid"])}
        )


def get_last_interaction(jid: str) -> str:
    """Get the most recent message involving the contact (sent by them, or in their direct chat)."""
    sender, chat = _contact_keys(jid)
    with _messages_db() as conn:
        row = conn.execute(
            f"""
            SELECT {MESSAGE_COLUMNS}
            FROM messages m
            LEFT JOIN chats c ON c.jid = m.chat_jid
            WHERE m.sender = ? OR m.chat_jid = ?
            ORDER BY m.timestamp DESC, m.rowid DESC
            LIMIT 1
            """,
            (sender, chat),
        ).fetchone()
        if not row:
            return f"No messages found involving {jid}."
        return _format_transcript(_Names(conn), [(row["chat_jid"], [(None, row)])])


# ---------------------------------------------------------------------------
# Chats
# ---------------------------------------------------------------------------

CHAT_COLUMNS = """
    c.jid, c.name, c.last_message_time,
    m.content, m.sender, m.is_from_me, m.media_type, m.filename, m.timestamp
"""

# Read state as the bridge records it: the point up to which I have read the chat on
# any device, and whether I marked it unread. Unread are other people's later messages.
READ_COLUMNS = """,
    c.last_read_at, c.marked_unread,
    CASE WHEN c.last_read_at IS NULL THEN NULL ELSE (
        SELECT COUNT(*) FROM messages u
        WHERE u.chat_jid = c.jid AND u.is_from_me = 0 AND u.timestamp > c.last_read_at
    ) END AS unread_count
"""
# A database written by a bridge from before read state was recorded
NO_READ_COLUMNS = ", NULL AS last_read_at, 0 AS marked_unread, NULL AS unread_count"
NO_READ_STATE = "Read state is not recorded yet: restart the WhatsApp bridge to update its database."

def _has_read_state(conn: sqlite3.Connection) -> bool:
    return conn.execute(
        "SELECT COUNT(*) FROM pragma_table_info('chats') WHERE name = 'last_read_at'"
    ).fetchone()[0] > 0

def _chat_columns(conn: sqlite3.Connection) -> str:
    return CHAT_COLUMNS + (READ_COLUMNS if _has_read_state(conn) else NO_READ_COLUMNS)

# The latest message per chat, picked deterministically (timestamps have whole-second ties).
LAST_MESSAGE_JOIN = """
    LEFT JOIN messages m ON m.rowid = (
        SELECT m2.rowid FROM messages m2 WHERE m2.chat_jid = c.jid
        ORDER BY m2.timestamp DESC, m2.rowid DESC LIMIT 1
    )
"""
NO_MESSAGE_JOIN = " LEFT JOIN messages m ON 0 "

def _chat_dict(row: sqlite3.Row, names: _Names, include_last_message: bool = True) -> Dict[str, Any]:
    jid = row["jid"]
    chat: Dict[str, Any] = {
        "jid": jid,
        "name": names.chat(jid, row["name"]),
        "is_group": jid.endswith(GROUP_SERVER),
        "last_message_time": _local_iso(row["last_message_time"] or row["timestamp"]),
        "unread_count": row["unread_count"],
        "marked_unread": bool(row["marked_unread"]),
        "last_read_time": _local_iso(row["last_read_at"]),
    }
    if include_last_message and row["timestamp"] is not None:
        text = _preview_text(row)
        if len(text) > PREVIEW_LENGTH:
            text = text[:PREVIEW_LENGTH] + "..."
        chat["last_message"] = text
        chat["last_sender"] = "Me" if row["is_from_me"] else names.sender(row["sender"])
        chat["last_is_from_me"] = bool(row["is_from_me"])
    return chat

def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
    unread_only: bool = False
) -> List[Dict[str, Any]]:
    """Get chats matching the specified criteria."""
    if sort_by not in ("last_active", "name"):
        raise WhatsAppError(f"Invalid sort_by: {sort_by!r}. Use 'last_active' or 'name'.")
    limit = _clamp(limit, 1, MAX_CHAT_LIMIT)
    page = max(0, int(page))

    with _messages_db() as conn:
        names = _Names(conn)
        if unread_only and not _has_read_state(conn):
            raise WhatsAppError(NO_READ_STATE)
        sql = f"SELECT {_chat_columns(conn)} FROM chats c"
        sql += LAST_MESSAGE_JOIN if include_last_message else NO_MESSAGE_JOIN
        where: List[str] = []
        params: List[Any] = []
        if query:
            # Also match people by their address-book name, which chats.name may not carry.
            needle = query.casefold()
            contact_jids = [jid for jid, name in names.contacts.items() if needle in name.casefold()][:500]
            placeholders = ",".join("?" * len(contact_jids))
            where.append(f"(c.name LIKE ? ESCAPE '\\' OR c.jid LIKE ? ESCAPE '\\' OR c.jid IN ({placeholders}))")
            params.extend([_like(query), _like(query)] + contact_jids)
        if unread_only:
            where.append("(unread_count > 0 OR c.marked_unread)")
        if where:
            sql += " WHERE " + " AND ".join(where)
        sql += " ORDER BY " + ("c.last_message_time DESC" if sort_by == "last_active" else "c.name COLLATE NOCASE")
        sql += ", c.jid LIMIT ? OFFSET ?"
        params.extend([limit, page * limit])
        return [_chat_dict(row, names, include_last_message) for row in conn.execute(sql, params)]


def get_chat(chat_jid: str, include_last_message: bool = True) -> Dict[str, Any]:
    """Get chat metadata by JID."""
    with _messages_db() as conn:
        sql = f"SELECT {_chat_columns(conn)} FROM chats c"
        sql += LAST_MESSAGE_JOIN if include_last_message else NO_MESSAGE_JOIN
        sql += " WHERE c.jid = ?"
        row = conn.execute(sql, (chat_jid.strip(),)).fetchone()
        if not row:
            raise WhatsAppError(f"Chat {chat_jid} not found.")
        return _chat_dict(row, _Names(conn), include_last_message)


def get_direct_chat_by_contact(sender_phone_number: str) -> Dict[str, Any]:
    """Get the direct chat with a phone number: exact number match first, then a unique partial match."""
    digits = _normalize_sender(sender_phone_number)
    with _messages_db() as conn:
        names = _Names(conn)
        base = f"SELECT {_chat_columns(conn)} FROM chats c {LAST_MESSAGE_JOIN}"
        exact_jid = digits if digits.endswith(LID_SERVER) else digits + USER_SERVER
        row = conn.execute(base + " WHERE c.jid = ?", (exact_jid,)).fetchone()
        if row:
            return _chat_dict(row, names)

        rows = conn.execute(
            base + " WHERE c.jid LIKE ? ESCAPE '\\' AND c.jid LIKE ? ORDER BY c.jid LIMIT 6",
            (_like(digits.split("@")[0]), "%" + USER_SERVER),
        ).fetchall()
        if not rows:
            raise WhatsAppError(f"No direct chat found for {sender_phone_number}.")
        if len(rows) > 1:
            candidates = ", ".join(f"{names.chat(r['jid'], r['name'])} ({r['jid']})" for r in rows[:5])
            raise WhatsAppError(
                f"No exact match for {sender_phone_number}, and several chats contain it: {candidates}. "
                "Use the full number with country code."
            )
        return _chat_dict(rows[0], names)


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    """Get all chats involving the contact: their direct chat and chats they sent messages in."""
    sender, chat = _contact_keys(jid)
    limit = _clamp(limit, 1, MAX_CHAT_LIMIT)
    page = max(0, int(page))
    with _messages_db() as conn:
        names = _Names(conn)
        cursor = conn.execute(
            f"""
            SELECT {_chat_columns(conn)}
            FROM chats c
            {LAST_MESSAGE_JOIN}
            WHERE c.jid = ?
               OR c.jid IN (SELECT DISTINCT chat_jid FROM messages WHERE sender = ?)
            ORDER BY c.last_message_time DESC, c.jid
            LIMIT ? OFFSET ?
            """,
            (chat, sender, limit, page * limit),
        )
        return [_chat_dict(row, names) for row in cursor]


UNREAD_CHAT_TYPES = ("all", "groups", "direct")
UNREAD_PER_CHAT = 50
MAX_UNREAD_CHATS = 50

def get_unread_messages(
    chat_jid: Optional[str] = None,
    limit: Optional[int] = None,
    context_before: int = 3,
    chat_type: str = "all",
    max_chats: int = 20,
) -> str:
    """Get the messages after the point I last read a chat, as a transcript: of one chat,
    or of every chat with unread messages when no chat is given."""
    context_before = _clamp(context_before, 0, MAX_CONTEXT)
    if chat_type not in UNREAD_CHAT_TYPES:
        raise WhatsAppError(f"Invalid chat_type: {chat_type!r}. Use 'all', 'groups' or 'direct'.")

    with _messages_db() as conn:
        names = _Names(conn)
        if chat_jid and chat_jid.strip():
            chat_jid = chat_jid.strip()
            chat = conn.execute(
                f"SELECT {_chat_columns(conn)} FROM chats c {NO_MESSAGE_JOIN} WHERE c.jid = ?", (chat_jid,)
            ).fetchone()
            if not chat:
                raise WhatsAppError(f"Chat {chat_jid} not found.")
            if not _has_read_state(conn):
                raise WhatsAppError(NO_READ_STATE)
            name = names.chat(chat_jid, chat["name"])
            if chat["last_read_at"] is None:
                raise WhatsAppError(
                    f"It is not known yet what has been read in {name}. It becomes known when the chat is next "
                    "read on the phone or replied to. Use list_messages with chat_jid and after instead."
                )
            per_chat = _clamp(200 if limit is None else limit, 1, MAX_MESSAGE_LIMIT)
            return _unread_section(conn, names, chat, per_chat, context_before)

        # Every chat with unread messages, most recently active first
        if not _has_read_state(conn):
            raise WhatsAppError(NO_READ_STATE)
        max_chats = _clamp(max_chats, 1, MAX_UNREAD_CHATS)
        per_chat = _clamp(UNREAD_PER_CHAT if limit is None else limit, 1, MAX_MESSAGE_LIMIT)
        where = "(unread_count > 0 OR c.marked_unread)"
        if chat_type == "groups":
            where += " AND c.jid LIKE '%@g.us'"
        elif chat_type == "direct":
            where += " AND c.jid NOT LIKE '%@g.us'"
        chats = conn.execute(
            f"SELECT {_chat_columns(conn)} FROM chats c {NO_MESSAGE_JOIN} WHERE {where} "
            "ORDER BY c.last_message_time DESC, c.jid"
        ).fetchall()
        kind = {"all": "chats", "groups": "group chats", "direct": "direct chats"}[chat_type]
        if not chats:
            return f"No unread messages in any {kind[:-1]}."
        total = len(chats)
        chats = chats[:max_chats]
        header = (
            f"Unread messages in {total} {kind}, most recently active first. "
            "Each chat starts with a \"## \" line; unread lines start with \"> \" and lines before them are "
            "earlier context."
        )
        if total > max_chats:
            header += (f" Only the first {max_chats} are shown; raise max_chats (up to {MAX_UNREAD_CHATS}) "
                       "or pass chat_jid for the rest.")
        sections = [
            f"## {names.chat(chat['jid'], chat['name'])} ({chat['jid']})\n"
            + _unread_section(conn, names, chat, per_chat, context_before)
            for chat in chats
        ]
        return header + "\n\n" + "\n\n".join(sections)


def _unread_section(conn: sqlite3.Connection, names: "_Names", chat: sqlite3.Row, limit: int, context_before: int) -> str:
    """The unread part of one chat whose read point is known, with a line saying how much is unread."""
    chat_jid = chat["jid"]
    name = names.chat(chat_jid, chat["name"])
    if chat["last_read_at"] is None:
        return (f"{name} is marked as unread, but it is not known yet what was read in it. "
                "Use list_messages with chat_jid and after to see it.")
    read_until = _local_iso(chat["last_read_at"])

    order = _chat_positions(conn, chat_jid)
    unread = [r[0] for r in conn.execute(
        "SELECT rowid FROM messages WHERE chat_jid = ? AND timestamp > ? ORDER BY timestamp ASC, rowid ASC",
        (chat_jid, chat["last_read_at"]),
    )]
    index = {rowid: i for i, rowid in enumerate(order)}

    if not unread:
        if not chat["marked_unread"]:
            return f"No unread messages in {name} (read up to {read_until})."
        # Marked unread by hand with nothing new: show where the conversation stands
        positions = list(range(max(0, len(order) - max(context_before, 5)), len(order)))
        header = f"{name} is marked as unread, but has no new messages since {read_until}. The latest messages:"
        marked = set()
    else:
        shown = unread[-limit:]
        first = index[shown[0]]
        # Context only helps when the unread part is shown from its start
        start = first - context_before if len(shown) == len(unread) else first
        positions = list(range(max(0, start), len(order)))
        header = (
            f"{chat['unread_count']} unread message(s) from others in {name} since {read_until}"
            + (" (also marked as unread)" if chat["marked_unread"] else "")
            + ". Unread lines start with \"> \"; lines before them are earlier context."
        )
        if len(shown) < len(unread):
            header += f" Showing the newest {len(shown)} of {len(unread)} messages after that point."
        marked = None

    rows = _fetch_rows(conn, [order[i] for i in positions])
    entries = [(i, rows[order[i]]) for i in positions if order[i] in rows]
    if marked is None:
        unread_set = set(unread)
        marked = {(row["id"], row["chat_jid"]) for i, row in entries if order[i] in unread_set}
    return header + "\n" + _format_transcript(names, [(chat_jid, entries)], marked)


# ---------------------------------------------------------------------------
# Polls
# ---------------------------------------------------------------------------

# Votes name the options they chose by the SHA-256 hash of the option text; a vote
# holds the voter's whole current choice, and an empty one means they withdrew it.
ADDED_LATER = "(an option added after the poll was sent)"
POLL_NOTE = "Only votes the bridge has seen are counted: those cast while it was linked, and those history brought."

def _has_polls(conn: sqlite3.Connection) -> bool:
    return conn.execute(
        "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'poll_votes'"
    ).fetchone()[0] > 0

def _load_polls(conn: sqlite3.Connection, keys: List[Tuple[str, str]]) -> Dict[Tuple[str, str], Dict[str, Any]]:
    """The polls among the given (message id, chat) keys, each with its votes."""
    if not keys or not _has_polls(conn):
        return {}
    polls: Dict[Tuple[str, str], Dict[str, Any]] = {}
    keys = list(dict.fromkeys(keys))
    for start in range(0, len(keys), 400):
        chunk = keys[start:start + 400]
        condition = " OR ".join("(message_id = ? AND chat_jid = ?)" for _ in chunk)
        params = [value for key in chunk for value in key]
        for row in conn.execute(
            f"SELECT message_id, chat_jid, question, options, selectable_count FROM polls WHERE {condition}", params
        ):
            try:
                options = [str(option) for option in json.loads(row["options"] or "[]")]
            except ValueError:
                options = []
            polls[(row["message_id"], row["chat_jid"])] = {
                "question": row["question"] or "",
                "options": options,
                "selectable": row["selectable_count"] or 0,
                "votes": [],
            }
    found = list(polls)
    for start in range(0, len(found), 400):
        chunk = found[start:start + 400]
        condition = " OR ".join("(poll_id = ? AND chat_jid = ?)" for _ in chunk)
        params = [value for key in chunk for value in key]
        for row in conn.execute(
            f"SELECT poll_id, chat_jid, voter, selected, voted_at, is_from_me FROM poll_votes WHERE {condition} "
            "ORDER BY voted_at, voter",
            params,
        ):
            try:
                selected = [str(h) for h in json.loads(row["selected"] or "[]")]
            except ValueError:
                selected = []
            polls[(row["poll_id"], row["chat_jid"])]["votes"].append({
                "voter": row["voter"],
                "is_from_me": bool(row["is_from_me"]),
                "selected": selected,
                "voted_at": row["voted_at"],
            })
    return polls

def _option_hash(option: str) -> str:
    return hashlib.sha256(option.encode("utf-8")).hexdigest()

def _vote_choices(poll: Dict[str, Any], vote: Dict[str, Any]) -> List[str]:
    by_hash = {_option_hash(option): option for option in poll["options"]}
    return [by_hash.get(h.lower(), ADDED_LATER) for h in vote["selected"]]

def _tally(poll: Dict[str, Any]) -> Tuple[List[Tuple[str, List[Dict[str, Any]]]], int]:
    """Each option with the votes that chose it (options added later last), and how many people voted."""
    chosen: Dict[str, List[Dict[str, Any]]] = {option: [] for option in poll["options"]}
    voters = 0
    for vote in poll["votes"]:
        choices = _vote_choices(poll, vote)
        if choices:
            voters += 1
        for choice in dict.fromkeys(choices):
            chosen.setdefault(choice, []).append(vote)
    return list(chosen.items()), voters

def _choose_text(selectable: int, option_count: int) -> str:
    if selectable == 1:
        return "choose one"
    if selectable <= 0 or selectable >= option_count:
        return "choose any"
    return f"choose up to {selectable}"

def _poll_lines(poll: Dict[str, Any]) -> str:
    """A poll in a transcript: the question, then one indented line per option with its count."""
    tally, voters = _tally(poll)
    lines = [f"📊 Poll: {_one_line(poll['question'])} ({_choose_text(poll['selectable'], len(poll['options']))})"]
    for option, votes in tally:
        lines.append(f"    • {_one_line(option)}: {len(votes)}")
    lines.append(f"    ({voters} voted)")
    return "\n".join(lines)

def _voter_name(names: _Names, vote: Dict[str, Any]) -> str:
    return "Me" if vote["is_from_me"] else names.sender(vote["voter"])

def get_poll_results(chat_jid: str, message_id: str) -> Dict[str, Any]:
    """Get a poll's question, options and who voted for what."""
    chat_jid = chat_jid.strip()
    with _messages_db() as conn:
        polls = _load_polls(conn, [(message_id, chat_jid)])
        poll = polls.get((message_id, chat_jid))
        if not poll:
            if not _has_polls(conn):
                raise WhatsAppError("Polls are not recorded yet: restart the WhatsApp bridge to update its database.")
            raise WhatsAppError(f"No poll with ID {message_id} in chat {chat_jid}.")
        names = _Names(conn)
        message = conn.execute(
            f"SELECT {MESSAGE_COLUMNS} FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid "
            "WHERE m.id = ? AND m.chat_jid = ?",
            (message_id, chat_jid),
        ).fetchone()
        tally, voters = _tally(poll)
        mine = next((vote for vote in poll["votes"] if vote["is_from_me"]), None)
        result: Dict[str, Any] = {
            "question": poll["question"],
            "chat": names.chat(chat_jid, message["chat_name"] if message else None),
            "chat_jid": chat_jid,
            "message_id": message_id,
            "choose": _choose_text(poll["selectable"], len(poll["options"])).replace("choose ", ""),
            "options": [
                {"option": option, "count": len(votes), "voters": [_voter_name(names, v) for v in votes]}
                for option, votes in tally
            ],
            "people_voted": voters,
            "my_choice": _vote_choices(poll, mine) if mine and mine["selected"] else None,
            "votes": [
                {
                    "voter": _voter_name(names, vote),
                    "choice": _vote_choices(poll, vote),
                    "voted_at": _local_iso(vote["voted_at"]),
                }
                for vote in poll["votes"]
            ],
            "note": POLL_NOTE + " A vote with an empty choice was withdrawn.",
        }
        if message:
            result["sent_by"] = "Me" if message["is_from_me"] else names.sender(message["sender"])
            result["sent_at"] = _local_iso(message["timestamp"])
        return result

def list_polls(chat_jid: Optional[str] = None, limit: int = 20) -> List[Dict[str, Any]]:
    """Get recent polls, newest first, with how many voted, what leads and whether I voted."""
    limit = _clamp(limit, 1, MAX_CHAT_LIMIT)
    with _messages_db() as conn:
        if not _has_polls(conn):
            raise WhatsAppError("Polls are not recorded yet: restart the WhatsApp bridge to update its database.")
        sql = (
            f"SELECT {MESSAGE_COLUMNS} FROM polls p "
            "JOIN messages m ON m.id = p.message_id AND m.chat_jid = p.chat_jid "
            "LEFT JOIN chats c ON c.jid = m.chat_jid"
        )
        params: List[Any] = []
        if chat_jid:
            sql += " WHERE m.chat_jid = ?"
            params.append(chat_jid.strip())
        sql += " ORDER BY m.timestamp DESC, m.rowid DESC LIMIT ?"
        params.append(limit)
        rows = conn.execute(sql, params).fetchall()
        polls = _load_polls(conn, [(row["id"], row["chat_jid"]) for row in rows])
        names = _Names(conn)
        result = []
        for row in rows:
            poll = polls[(row["id"], row["chat_jid"])]
            tally, voters = _tally(poll)
            top = max((len(votes) for _, votes in tally), default=0)
            leading = [option for option, votes in tally if top and len(votes) == top]
            result.append({
                "chat": names.chat(row["chat_jid"], row["chat_name"]),
                "chat_jid": row["chat_jid"],
                "message_id": row["id"],
                "question": poll["question"],
                "sent_by": "Me" if row["is_from_me"] else names.sender(row["sender"]),
                "sent_at": _local_iso(row["timestamp"]),
                "people_voted": voters,
                "leading": leading,
                "leading_votes": top,
                "i_voted": any(vote["is_from_me"] and vote["selected"] for vote in poll["votes"]),
            })
        return result


def search_contacts(query: str) -> List[Dict[str, Any]]:
    """Search people by name or phone number, across direct chats and the WhatsApp address book."""
    query = (query or "").strip()
    if not query:
        raise WhatsAppError("Search query must not be empty.")
    needle = query.casefold()
    # '+27 82 123-4567' should match the stored digits.
    compact = "".join(ch for ch in query if ch not in " +-()")
    number = compact if compact.isdigit() else None

    with _messages_db() as conn:
        names = _Names(conn)
        results: Dict[str, Dict[str, Any]] = {}

        def add(jid: str, name: str, has_chat: bool) -> None:
            results[jid] = {
                "jid": jid,
                "name": name,
                "phone_number": jid.split("@")[0] if jid.endswith(USER_SERVER) else None,
                "has_chat": has_chat,
            }

        direct_chats = {
            row["jid"]: row["name"]
            for row in conn.execute("SELECT jid, name FROM chats WHERE jid NOT LIKE ?", ("%" + GROUP_SERVER,))
        }

        def matches(jid: str, name: str) -> bool:
            if needle in name.casefold():
                return True
            return bool(number) and jid.endswith(USER_SERVER) and number in jid.split("@")[0]

        for jid, chat_name in direct_chats.items():
            name = names.chat(jid, chat_name)
            if matches(jid, name) or (chat_name and needle in chat_name.casefold()):
                add(jid, name, True)

        for jid, name in names.contacts.items():
            if jid in results or jid.endswith(GROUP_SERVER):
                continue
            if matches(jid, name):
                add(jid, name, jid in direct_chats)

        ordered = sorted(results.values(), key=lambda c: (not c["has_chat"], c["name"].casefold(), c["jid"]))
        return ordered[:50]


# ---------------------------------------------------------------------------
# Bridge HTTP API
# ---------------------------------------------------------------------------

def _bridge_post(endpoint: str, payload: Dict[str, Any], timeout: int) -> Dict[str, Any]:
    """POST to the local bridge. Always returns a dict with 'success' and 'message'."""
    token_path = api_token_path()
    try:
        with open(token_path, "r", encoding="utf-8") as f:
            token = f.read().strip()
    except OSError:
        token = ""
    if not token:
        return {
            "success": False,
            "message": f"WhatsApp bridge is not running or not paired yet (no API token at {token_path}). "
                       "Start whatsapp-bridge and scan the QR code, then try again.",
        }

    url = f"{api_base_url()}/{endpoint}"
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    try:
        with requests.Session() as session:
            session.trust_env = False  # never route the token through a proxy from the environment
            response = session.post(url, data=json.dumps(payload), headers=headers, timeout=timeout)
    except requests.ConnectionError:
        return {
            "success": False,
            "message": f"WhatsApp bridge is not running (could not connect to {url}). Start whatsapp-bridge and try again.",
        }
    except requests.Timeout:
        return {"success": False, "message": f"WhatsApp bridge did not respond within {timeout} seconds."}
    except requests.RequestException as e:
        return {"success": False, "message": f"Request to WhatsApp bridge failed: {e}"}

    try:
        result = response.json()
    except ValueError:
        result = None
    if not isinstance(result, dict):
        return {
            "success": False,
            "message": f"Unexpected response from WhatsApp bridge (HTTP {response.status_code}): {response.text[:200]}",
        }

    # Error responses carry the bridge's own explanation: pass it through verbatim.
    result["success"] = bool(result.get("success", False)) and response.ok
    result["message"] = str(result.get("message") or f"HTTP {response.status_code} from WhatsApp bridge")
    return result

def _send_result(result: Dict[str, Any]) -> Dict[str, Any]:
    return {"success": result["success"], "message": result["message"]}

def _failure(message: str) -> Dict[str, Any]:
    return {"success": False, "message": message}

def send_message(recipient: str, message: str) -> Dict[str, Any]:
    if not recipient:
        return _failure("Recipient must be provided")
    if not message:
        return _failure("Message must be provided")
    return _send_result(_bridge_post("send", {"recipient": recipient, "message": message}, SEND_TIMEOUT))

def send_poll(recipient: str, question: str, options: List[str], multiple_answers: bool = False) -> Dict[str, Any]:
    """Ask the bridge to send a poll; it checks the question and options against WhatsApp's limits."""
    if not recipient:
        return _failure("Recipient must be provided")
    if not question or not question.strip():
        return _failure("Question must be provided")
    if not isinstance(options, list) or not all(isinstance(option, str) for option in options):
        return _failure("Options must be a list of texts")
    payload = {
        "recipient": recipient,
        "question": question,
        "options": options,
        "multiple_answers": bool(multiple_answers),
    }
    result = _bridge_post("send_poll", payload, SEND_TIMEOUT)
    sent = _send_result(result)
    if result["success"] and result.get("message_id"):
        sent["message_id"] = str(result["message_id"])
    return sent

def send_file(recipient: str, media_path: str) -> Dict[str, Any]:
    if not recipient:
        return _failure("Recipient must be provided")
    if not media_path:
        return _failure("Media path must be provided")
    if not os.path.isfile(media_path):
        return _failure(f"Media file not found: {media_path}")
    payload = {"recipient": recipient, "message": "", "media_path": os.path.abspath(media_path)}
    return _send_result(_bridge_post("send", payload, SEND_MEDIA_TIMEOUT))

def send_audio_message(recipient: str, media_path: str) -> Dict[str, Any]:
    if not recipient:
        return _failure("Recipient must be provided")
    if not media_path:
        return _failure("Media path must be provided")
    if not os.path.isfile(media_path):
        return _failure(f"Media file not found: {media_path}")

    temp_path = None
    try:
        if not media_path.endswith(".ogg"):
            try:
                temp_path = audio.convert_to_opus_ogg_temp(media_path)
            except Exception as e:
                return _failure(f"Error converting file to opus ogg. You likely need to install ffmpeg: {e}")
        payload = {"recipient": recipient, "message": "", "media_path": os.path.abspath(temp_path or media_path)}
        return _send_result(_bridge_post("send", payload, SEND_MEDIA_TIMEOUT))
    finally:
        if temp_path and os.path.exists(temp_path):
            try:
                os.unlink(temp_path)
            except OSError as e:
                logger.warning("Could not delete temporary file %s: %s", temp_path, e)

def download_media(message_id: str, chat_jid: str) -> Dict[str, Any]:
    """Ask the bridge to download a message's media; returns success, message and the local file path."""
    if not message_id or not chat_jid:
        return _failure("Both message_id and chat_jid must be provided")
    result = _bridge_post("download", {"message_id": message_id, "chat_jid": chat_jid}, DOWNLOAD_TIMEOUT)
    output = _send_result(result)
    if result["success"]:
        if not result.get("path"):
            return _failure("WhatsApp bridge reported success but returned no file path")
        output["file_path"] = result["path"]
        if result.get("filename"):
            output["filename"] = result["filename"]
    return output
