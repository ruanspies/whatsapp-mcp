"""Fixtures: a throwaway bridge store (messages.db, whatsapp.db, api_token) and a stub bridge HTTP API."""
import json
import os
import sqlite3
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

# Fixed, DST-free local zone (UTC+02:00) so local-time assertions are deterministic.
os.environ["TZ"] = "Africa/Johannesburg"
time.tzset()
for _var in ("WHATSAPP_MCP_READ_ONLY", "WHATSAPP_MCP_ENABLE_SEND_MEDIA"):
    os.environ.pop(_var, None)

TOKEN = "test-token-123"
GROUP = "120363000000000001@g.us"
ALICE = "27821234567@s.whatsapp.net"
BOB = "27829999999@s.whatsapp.net"
LOOKALIKE = "127821234567@s.whatsapp.net"

SCHEMA = """
CREATE TABLE chats(jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP,
    last_read_at TIMESTAMP, marked_unread INTEGER NOT NULL DEFAULT 0);
CREATE TABLE messages(id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, direct_path TEXT, mime_type TEXT, PRIMARY KEY (id, chat_jid));
"""

# (id, chat, sender, content, UTC time, is_from_me, media_type, filename)
MESSAGES = [
    # Group, day 1 (local 2026-10-08)
    ("G1", GROUP, "27821234567", "Who is bringing the braai wood?", "2026-10-08 08:00:00+00:00", 0, None, None),
    ("G2", GROUP, "27829999999", "I can bring wood", "2026-10-08 08:01:00+00:00", 0, None, None),
    ("G3", GROUP, "27820000001", "Thanks all", "2026-10-08 08:01:00+00:00", 1, None, None),
    ("G4", GROUP, "99887766@lid", "Running late", "2026-10-08 12:00:00+00:00", 0, None, None),
    # 22:30 UTC on the 8th is 00:30 local on the 9th
    ("G5", GROUP, "27821234567", "Midnight thought about the budget", "2026-10-08 22:30:00+00:00", 0, None, None),
    # Group, day 2
    ("G6", GROUP, "11223344@lid", "", "2026-10-09 09:00:00+00:00", 0, "image", None),
    ("DUP1", GROUP, "27829999999", "Budget spreadsheet attached", "2026-10-09 09:05:00+00:00", 0, "document", "budget.xlsx"),
    ("G8", GROUP, "27821234567", "Looks good to me", "2026-10-09 09:06:00+00:00", 0, None, None),
    ("G9", GROUP, "27820000001", "See you Saturday", "2026-10-09 15:00:00+00:00", 1, None, None),
    # Direct chats
    ("A1", ALICE, "27821234567", "Lunch tomorrow?", "2026-10-08 10:00:00+00:00", 0, None, None),
    ("DUP1", ALICE, "27820000001", "Yes, 12:30", "2026-10-08 10:05:00+00:00", 1, None, None),
    ("A3", ALICE, "27821234567", "Great. Did you see the budget?", "2026-10-09 07:00:00+00:00", 0, None, None),
    ("B1", BOB, "27829999999", "Call me when you can", "2026-10-09 16:00:00+00:00", 0, None, None),
    ("L1", LOOKALIKE, "127821234567", "Wrong number sorry", "2026-10-07 06:00:00+00:00", 0, None, None),
]

# (jid, name, last_message_time, last_read_at, marked_unread)
CHATS = [
    # Read, but marked unread by hand
    (LOOKALIKE, "Wrong Person", "2026-10-07 06:00:00+00:00", "2026-10-07 06:00:00+00:00", 1),
    # Read up to G5: G6, DUP1 and G8 from others are unread; G9 is mine
    (GROUP, "Family", "2026-10-09 15:00:00+00:00", "2026-10-08 22:30:00+00:00", 0),
    # Read up to my reply: A3 is unread
    (ALICE, "Alice Chat", "2026-10-09 07:00:00+00:00", "2026-10-08 10:05:00+00:00", 0),
    # Not known yet
    (BOB, "27829999999", "2026-10-09 16:00:00+00:00", None, 0),  # name fell back to the number
]

# (their_jid, first_name, full_name, push_name, business_name)
CONTACTS = [
    (ALICE, "Alice", "Alice Smith", "ali", None),
    (BOB, None, None, "Bob", None),
    ("27825550000@s.whatsapp.net", "Carol", "Carol Jones", None, None),  # no chat yet
    ("99887766@lid", None, None, "Dave", None),
]


def build_store(path, with_contacts=True, with_token=True):
    os.makedirs(path, exist_ok=True)
    conn = sqlite3.connect(os.path.join(path, "messages.db"))
    conn.executescript(SCHEMA)
    conn.executemany("INSERT INTO chats VALUES (?, ?, ?, ?, ?)", CHATS)
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) "
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
        MESSAGES,
    )
    conn.commit()
    conn.close()
    if with_contacts:
        conn = sqlite3.connect(os.path.join(path, "whatsapp.db"))
        conn.execute(
            "CREATE TABLE whatsmeow_contacts(our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, "
            "push_name TEXT, business_name TEXT, redacted_phone TEXT)"
        )
        conn.executemany(
            "INSERT INTO whatsmeow_contacts (our_jid, their_jid, first_name, full_name, push_name, business_name) "
            "VALUES ('27820000001@s.whatsapp.net', ?, ?, ?, ?, ?)",
            CONTACTS,
        )
        conn.commit()
        conn.close()
    if with_token:
        with open(os.path.join(path, "api_token"), "w") as f:
            f.write(TOKEN + "\n")
    return str(path)


class StubBridge(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        self.server.requests.append({"path": self.path, "headers": dict(self.headers), "body": body})
        if self.headers.get("Authorization") != f"Bearer {TOKEN}":
            return self._reply(401, {"success": False, "message": "unauthorized"})
        if self.path == "/api/send":
            if body.get("recipient") == "27820009999":
                return self._reply(403, {"success": False, "message": "recipient is not an existing chat"})
            return self._reply(200, {"success": True, "message": f"Message sent to {body.get('recipient')}"})
        if self.path == "/api/download":
            if body.get("message_id") == "HUGE":
                return self._reply(413, {"success": False, "message": "media exceeds the download size limit"})
            return self._reply(200, {"success": True, "message": "Successfully downloaded document media",
                                     "filename": "budget.xlsx", "path": "/tmp/store/media/budget.xlsx"})
        return self._reply(404, {"success": False, "message": "not found"})


@pytest.fixture
def bridge():
    server = ThreadingHTTPServer(("127.0.0.1", 0), StubBridge)
    server.requests = []
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield server
    server.shutdown()
    server.server_close()


@pytest.fixture
def store(tmp_path, bridge, monkeypatch):
    path = build_store(tmp_path / "store")
    monkeypatch.setenv("WHATSAPP_MCP_STORE_DIR", path)
    monkeypatch.setenv("WHATSAPP_MCP_PORT", str(bridge.server_address[1]))
    return path
