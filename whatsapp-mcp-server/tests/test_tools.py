"""Every tool exercised in-process through the MCP server object (default registration mode)."""
import asyncio
import os
import socket
import sqlite3

import pytest
from mcp.server.mcpserver.exceptions import ToolError, UnexpectedToolError

import main
import whatsapp
from conftest import ALICE, BOB, GROUP, LOOKALIKE, TOKEN, build_store


def call(name, **arguments):
    """Call a tool the way the protocol layer does, returning its (validated) structured result."""
    result = asyncio.run(main.mcp.call_tool(name, arguments))
    assert not result.is_error
    if result.structured_content is None:  # plain-text tools
        return "\n".join(block.text for block in result.content)
    return result.structured_content["result"]


def error(name, **arguments):
    with pytest.raises(ToolError) as info:
        asyncio.run(main.mcp.call_tool(name, arguments))
    assert not isinstance(info.value, UnexpectedToolError), info.value.__cause__
    return str(info.value)


def ids(transcript):
    return [line.split("id=")[1].split("]")[0] for line in transcript.splitlines() if "id=" in line]


def test_default_tool_set():
    names = [tool.name for tool in asyncio.run(main.mcp.list_tools())]
    assert names == [
        "search_contacts", "list_messages", "list_chats", "get_chat", "get_direct_chat_by_contact",
        "get_contact_chats", "get_last_interaction", "get_message_context", "send_message", "download_media",
    ]


def test_no_print_on_request_path():
    for module in (main, whatsapp):
        with open(module.__file__) as f:
            assert "print(" not in f.read()


# --- list_messages ---------------------------------------------------------

def test_transcript_is_chronological_with_local_times(store):
    out = call("list_messages", chat_jid=GROUP)
    assert out == "\n".join([
        f"Chat: Family ({GROUP}, group)",
        "-- Thu 2026-10-08 UTC+02:00 --",
        "[10:00 id=G1] Alice Smith: Who is bringing the braai wood?",
        "[10:01 id=G2] Bob: I can bring wood",
        "[10:01 id=G3] Me: Thanks all",
        "[14:00 id=G4] Dave: Running late",
        "-- Fri 2026-10-09 UTC+02:00 --",
        "[00:30 id=G5] Alice Smith: Midnight thought about the budget",
        "[11:00 id=G6] unknown (11223344@lid): [image]",
        "[11:05 id=DUP1] Bob: [document: budget.xlsx] Budget spreadsheet attached",
        "[11:06 id=G8] Alice Smith: Looks good to me",
        "[17:00 id=G9] Me: See you Saturday",
    ])


def test_limit_selects_newest_and_pages_backwards(store):
    out = call("list_messages", chat_jid=GROUP, limit=3)
    assert ids(out) == ["DUP1", "G8", "G9"]
    assert out.splitlines()[0].startswith("(Showing the newest 3 matching messages; older ones exist. Use page=1")
    assert ids(call("list_messages", chat_jid=GROUP, limit=3, page=1)) == ["G4", "G5", "G6"]
    assert call("list_messages", chat_jid=GROUP, limit=3, page=9) == "No more messages."


def test_limit_is_capped(store):
    assert len(ids(call("list_messages", limit=100000))) == 14
    assert whatsapp._clamp(100000, 1, whatsapp.MAX_MESSAGE_LIMIT) == 500


def test_date_filters(store):
    # Naive = local time (UTC+02:00): local midnight on the 9th is 22:00 UTC on the 8th.
    assert ids(call("list_messages", chat_jid=GROUP, after="2026-10-09")) == ["G5", "G6", "DUP1", "G8", "G9"]
    assert ids(call("list_messages", chat_jid=GROUP, after="2026-10-09T00:00:00")) == ["G5", "G6", "DUP1", "G8", "G9"]
    # Explicit offsets are honoured.
    assert ids(call("list_messages", chat_jid=GROUP, after="2026-10-09T00:00:00+00:00")) == ["G6", "DUP1", "G8", "G9"]
    assert ids(call("list_messages", chat_jid=GROUP, after="2026-10-09T00:00:00Z")) == ["G6", "DUP1", "G8", "G9"]
    assert ids(call("list_messages", chat_jid=GROUP, after="2026-10-09T02:30:00+04:00")) == ["G5", "G6", "DUP1", "G8", "G9"]
    # before is exclusive; 14:00 local is 12:00 UTC.
    assert ids(call("list_messages", chat_jid=GROUP, before="2026-10-08T14:00")) == ["G1", "G2", "G3"]
    assert ids(call("list_messages", chat_jid=GROUP, after="2026-10-08T14:00", before="2026-10-09T11:05")) == ["G4", "G5", "G6"]
    assert "Invalid date format for 'after'" in error("list_messages", after="yesterday")


def test_sender_filter_normalises_numbers(store):
    expected = ["G1", "G5", "G8"]
    for sender in ("27821234567", "+27 82 123 4567", "27821234567@s.whatsapp.net"):
        assert ids(call("list_messages", chat_jid=GROUP, sender_phone_number=sender)) == expected
    assert ids(call("list_messages", sender_phone_number="99887766@lid")) == ["G4"]
    assert "Invalid phone number" in error("list_messages", sender_phone_number="alice")


def test_query_adds_deduplicated_context_per_chat(store):
    out = call("list_messages", query="budget")
    assert out == "\n".join([
        f"Chat: Alice Smith ({ALICE}, direct)",
        "-- Thu 2026-10-08 UTC+02:00 --",
        "[12:05 id=DUP1] Me: Yes, 12:30",
        "-- Fri 2026-10-09 UTC+02:00 --",
        "> [09:00 id=A3] Alice Smith: Great. Did you see the budget?",
        "",
        f"Chat: Family ({GROUP}, group)",
        "-- Thu 2026-10-08 UTC+02:00 --",
        "[14:00 id=G4] Dave: Running late",
        "-- Fri 2026-10-09 UTC+02:00 --",
        "> [00:30 id=G5] Alice Smith: Midnight thought about the budget",
        "[11:00 id=G6] unknown (11223344@lid): [image]",   # shared by two matches, printed once
        "> [11:05 id=DUP1] Bob: [document: budget.xlsx] Budget spreadsheet attached",
        "[11:06 id=G8] Alice Smith: Looks good to me",
    ])


def test_query_marks_gaps_and_can_skip_context(store):
    out = call("list_messages", chat_jid=GROUP, query="wood", context_before=0, context_after=0)
    assert ids(out) == ["G1", "G2"] and ">" not in out
    out = call("list_messages", chat_jid=GROUP, query="e", include_context=False)
    assert ">" not in out and "..." not in out
    lines = call("list_messages", chat_jid=GROUP, query="saturday").splitlines()
    assert lines[-2:] == ["[11:06 id=G8] Alice Smith: Looks good to me", "> [17:00 id=G9] Me: See you Saturday"]
    lines = call("list_messages", chat_jid=GROUP, query="braai wood", context_after=0).splitlines()
    assert lines[-1].startswith("> [10:00 id=G1]")
    out = call("list_messages", chat_jid=GROUP, query="a", sender_phone_number="27829999999", context_before=0, context_after=1)
    assert out.splitlines()[2:] == [
        "> [10:01 id=G2] Bob: I can bring wood",
        "[10:01 id=G3] Me: Thanks all",
        "...",
        "-- Fri 2026-10-09 UTC+02:00 --",
        "> [11:05 id=DUP1] Bob: [document: budget.xlsx] Budget spreadsheet attached",
        "[11:06 id=G8] Alice Smith: Looks good to me",
    ]


def test_context_is_not_added_without_query(store):
    assert ids(call("list_messages", chat_jid=GROUP, sender_phone_number="99887766@lid", context_before=5)) == ["G4"]


def test_like_wildcards_are_literal(store):
    assert call("list_messages", query="%") == "No messages found."
    assert call("list_messages", query="_") == "No messages found."


def test_no_messages(store):
    assert call("list_messages", chat_jid="nobody@g.us") == "No messages found."


# --- get_message_context / get_last_interaction ------------------------------

def test_message_context_keys_on_id_and_chat(store):
    message = error("get_message_context", message_id="DUP1")
    assert "exists in several chats" in message and GROUP in message and ALICE in message and "chat_jid" in message

    out = call("get_message_context", message_id="DUP1", chat_jid=ALICE, before=1, after=1)
    assert out.splitlines() == [
        f"Chat: Alice Smith ({ALICE}, direct)",
        "-- Thu 2026-10-08 UTC+02:00 --",
        "[12:00 id=A1] Alice Smith: Lunch tomorrow?",
        "> [12:05 id=DUP1] Me: Yes, 12:30",
        "-- Fri 2026-10-09 UTC+02:00 --",
        "[09:00 id=A3] Alice Smith: Great. Did you see the budget?",
    ]
    assert ids(call("get_message_context", message_id="DUP1", chat_jid=GROUP, before=2, after=1)) == ["G5", "G6", "DUP1", "G8"]
    # Unique id: chat_jid is optional. Same-second neighbours are not lost.
    assert ids(call("get_message_context", message_id="G2", before=1, after=1)) == ["G1", "G2", "G3"]
    assert "not found" in error("get_message_context", message_id="NOPE")
    assert "not found in chat" in error("get_message_context", message_id="G2", chat_jid=ALICE)


def test_last_interaction(store):
    out = call("get_last_interaction", jid="27829999999")
    assert out.splitlines() == [f"Chat: Bob ({BOB}, direct)", "-- Fri 2026-10-09 UTC+02:00 --", "[18:00 id=B1] Bob: Call me when you can"]
    assert ids(call("get_last_interaction", jid=ALICE)) == ["G8"]
    assert ids(call("get_last_interaction", jid=GROUP)) == ["G9"]
    assert call("get_last_interaction", jid="27810000000") == "No messages found involving 27810000000."


# --- chats and contacts ----------------------------------------------------

def test_list_chats(store):
    chats = call("list_chats")
    assert [c["jid"] for c in chats] == [BOB, GROUP, ALICE, LOOKALIKE]
    assert chats[0] == {
        "jid": BOB, "name": "Bob", "is_group": False, "last_message_time": "2026-10-09T18:00:00+02:00",
        "last_message": "Call me when you can", "last_sender": "Bob", "last_is_from_me": False,
    }
    assert chats[1]["is_group"] and chats[1]["last_sender"] == "Me" and chats[1]["last_is_from_me"] is True
    assert [c["jid"] for c in call("list_chats", sort_by="name", limit=2, page=1)] == [GROUP, LOOKALIKE]
    assert [c["jid"] for c in call("list_chats", query="fam")] == [GROUP]
    assert [c["jid"] for c in call("list_chats", query="alice smith")] == [ALICE]  # address-book name
    assert "last_message" not in call("list_chats", include_last_message=False)[0]
    assert "Invalid sort_by" in error("list_chats", sort_by="jid; DROP TABLE chats")


def test_get_chat(store):
    chat = call("get_chat", chat_jid=GROUP)
    assert chat["name"] == "Family" and chat["last_message"] == "See you Saturday"
    assert "last_message" not in call("get_chat", chat_jid=GROUP, include_last_message=False)
    assert "not found" in error("get_chat", chat_jid="nobody@g.us")


def test_direct_chat_prefers_exact_number(store):
    for number in ("27821234567", "+27 82 123 4567", ALICE):
        assert call("get_direct_chat_by_contact", sender_phone_number=number)["jid"] == ALICE
    assert call("get_direct_chat_by_contact", sender_phone_number="99999")["jid"] == BOB  # unique partial
    message = error("get_direct_chat_by_contact", sender_phone_number="2782")
    assert "several chats" in message and ALICE in message and BOB in message
    assert "No direct chat found" in error("get_direct_chat_by_contact", sender_phone_number="4400000")


def test_contact_chats(store):
    assert [c["jid"] for c in call("get_contact_chats", jid=ALICE)] == [GROUP, ALICE]
    assert [c["jid"] for c in call("get_contact_chats", jid="+27 82 999 9999")] == [BOB, GROUP]
    assert [c["jid"] for c in call("get_contact_chats", jid="99887766@lid")] == [GROUP]
    assert call("get_contact_chats", jid="27810000000") == []


def test_search_contacts(store):
    assert call("search_contacts", query="alice") == [
        {"jid": ALICE, "name": "Alice Smith", "phone_number": "27821234567", "has_chat": True}
    ]
    assert call("search_contacts", query="CAROL") == [
        {"jid": "27825550000@s.whatsapp.net", "name": "Carol Jones", "phone_number": "27825550000", "has_chat": False}
    ]
    assert [c["jid"] for c in call("search_contacts", query="+27 82 999")] == [BOB]
    assert [c["jid"] for c in call("search_contacts", query="2782123")] == [ALICE, LOOKALIKE]
    assert call("search_contacts", query="dave") == [
        {"jid": "99887766@lid", "name": "Dave", "phone_number": None, "has_chat": False}
    ]
    assert call("search_contacts", query="family") == []  # groups are not contacts
    assert call("search_contacts", query="zzz") == []


def test_names_fall_back_without_contacts_db(tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_MCP_STORE_DIR", build_store(tmp_path / "s", with_contacts=False))
    lines = call("list_messages", chat_jid=GROUP, before="2026-10-08T15:00").splitlines()
    assert lines[2:] == [
        "[10:00 id=G1] Alice Chat: Who is bringing the braai wood?",   # chats.name
        "[10:01 id=G2] 27829999999: I can bring wood",                 # name equal to number -> number
        "[10:01 id=G3] Me: Thanks all",
        "[14:00 id=G4] unknown (99887766@lid): Running late",          # never shown as a phone number
    ]
    assert call("search_contacts", query="alice")[0]["name"] == "Alice Chat"


def test_names_fall_back_on_unexpected_contacts_schema(tmp_path, monkeypatch):
    path = build_store(tmp_path / "s", with_contacts=False)
    for ddl in ("CREATE TABLE other(x)", "CREATE TABLE whatsmeow_contacts(jid TEXT, nickname TEXT)"):
        db = os.path.join(path, "whatsapp.db")
        if os.path.exists(db):
            os.unlink(db)
        conn = sqlite3.connect(db)
        conn.execute(ddl)
        if "whatsmeow" in ddl:
            conn.execute("INSERT INTO whatsmeow_contacts VALUES ('x@s.whatsapp.net', 'X')")
        conn.commit()
        conn.close()
        monkeypatch.setenv("WHATSAPP_MCP_STORE_DIR", path)
        assert "[10:00 id=G1] Alice Chat:" in call("list_messages", chat_jid=GROUP)


# --- database errors ---------------------------------------------------------

READ_TOOLS = [
    ("search_contacts", {"query": "a"}), ("list_messages", {}), ("list_chats", {}), ("get_chat", {"chat_jid": GROUP}),
    ("get_direct_chat_by_contact", {"sender_phone_number": "27821234567"}), ("get_contact_chats", {"jid": ALICE}),
    ("get_last_interaction", {"jid": ALICE}), ("get_message_context", {"message_id": "G1"}),
]


@pytest.mark.parametrize("name,arguments", READ_TOOLS)
def test_missing_database_is_a_clear_error(tmp_path, monkeypatch, name, arguments):
    monkeypatch.setenv("WHATSAPP_MCP_STORE_DIR", str(tmp_path / "empty"))
    message = error(name, **arguments)
    assert "message database not found" in message and "has not been run and paired yet" in message


@pytest.mark.parametrize("name,arguments", READ_TOOLS)
def test_database_errors_surface(tmp_path, monkeypatch, name, arguments):
    os.makedirs(tmp_path / "bad")
    sqlite3.connect(tmp_path / "bad" / "messages.db").executescript("CREATE TABLE unrelated(x);")
    monkeypatch.setenv("WHATSAPP_MCP_STORE_DIR", str(tmp_path / "bad"))
    assert "Database error" in error(name, **arguments)


def test_database_is_opened_read_only(store):
    with whatsapp._messages_db() as conn:
        with pytest.raises(sqlite3.OperationalError, match="readonly"):
            conn.execute("DELETE FROM messages")


# --- bridge HTTP API ---------------------------------------------------------

def test_send_message(store, bridge):
    assert call("send_message", recipient="27821234567", message="On my way") == {
        "success": True, "message": "Message sent to 27821234567"
    }
    request = bridge.requests[-1]
    assert request["path"] == "/api/send"
    assert request["body"] == {"recipient": "27821234567", "message": "On my way"}
    assert request["headers"]["Authorization"] == f"Bearer {TOKEN}"
    assert request["headers"]["Content-Type"] == "application/json"
    assert "Origin" not in request["headers"]
    assert request["headers"]["Host"].startswith("127.0.0.1:")


def test_bridge_error_message_is_passed_through(store):
    assert call("send_message", recipient="27820009999", message="hi") == {
        "success": False, "message": "recipient is not an existing chat"
    }
    assert call("download_media", message_id="HUGE", chat_jid=GROUP) == {
        "success": False, "message": "media exceeds the download size limit"
    }


def test_send_message_validates_input_locally(store, bridge):
    assert call("send_message", recipient="", message="hi")["message"] == "Recipient must be provided"
    assert call("send_message", recipient="27821234567", message="")["message"] == "Message must be provided"
    assert bridge.requests == []


def test_token_is_read_fresh_on_each_call(store):
    assert call("send_message", recipient="27821234567", message="hi")["success"]
    with open(os.path.join(store, "api_token"), "w") as f:
        f.write("rotated")
    assert call("send_message", recipient="27821234567", message="hi") == {"success": False, "message": "unauthorized"}


def test_missing_token(store, bridge):
    os.unlink(os.path.join(store, "api_token"))
    for result in (call("send_message", recipient="27821234567", message="hi"),
                   call("download_media", message_id="DUP1", chat_jid=GROUP)):
        assert result["success"] is False
        assert "not running or not paired yet" in result["message"] and "api_token" in result["message"]
    assert bridge.requests == []


def test_bridge_down(store, monkeypatch):
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        closed_port = s.getsockname()[1]
    monkeypatch.setenv("WHATSAPP_MCP_PORT", str(closed_port))
    for result in (call("send_message", recipient="27821234567", message="hi"),
                   call("download_media", message_id="DUP1", chat_jid=GROUP)):
        assert result["success"] is False
        assert "WhatsApp bridge is not running" in result["message"]


def test_download_media(store, bridge):
    assert call("download_media", message_id="DUP1", chat_jid=GROUP) == {
        "success": True, "message": "Successfully downloaded document media",
        "file_path": "/tmp/store/media/budget.xlsx", "filename": "budget.xlsx",
    }
    assert bridge.requests[-1]["path"] == "/api/download"
    assert bridge.requests[-1]["body"] == {"message_id": "DUP1", "chat_jid": GROUP}


def test_send_file_and_audio(store, bridge, tmp_path, monkeypatch):
    assert whatsapp.send_file(GROUP, str(tmp_path / "missing.pdf"))["message"].startswith("Media file not found")
    document = tmp_path / "doc.pdf"
    document.write_bytes(b"%PDF")
    assert whatsapp.send_file(GROUP, str(document))["success"]
    assert bridge.requests[-1]["body"] == {"recipient": GROUP, "message": "", "media_path": str(document)}

    converted = tmp_path / "converted.ogg"

    def fake_convert(path):
        converted.write_bytes(b"OggS")
        return str(converted)

    monkeypatch.setattr(whatsapp.audio, "convert_to_opus_ogg_temp", fake_convert)
    voice = tmp_path / "voice.mp3"
    voice.write_bytes(b"ID3")
    assert whatsapp.send_audio_message(GROUP, str(voice))["success"]
    assert bridge.requests[-1]["body"]["media_path"] == str(converted)
    assert not converted.exists()          # temp .ogg removed after sending
    assert voice.exists()

    original = tmp_path / "note.ogg"       # an .ogg the caller owns is sent as-is and kept
    original.write_bytes(b"OggS")
    assert whatsapp.send_audio_message(GROUP, str(original))["success"]
    assert original.exists()
