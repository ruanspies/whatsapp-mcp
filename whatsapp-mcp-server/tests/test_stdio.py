"""The real server process over stdio: registration modes, error results, and a clean stdout."""
import json
import os
import select
import subprocess
import sys

import pytest

from conftest import ALICE, GROUP, build_store

SERVER_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SEND_TOOLS = {"send_message", "send_poll", "send_file", "send_audio_message"}


class Server:
    def __init__(self, env):
        full_env = {k: v for k, v in os.environ.items() if not k.startswith("WHATSAPP_MCP_")}
        full_env.update(env)
        self.proc = subprocess.Popen(
            [sys.executable, os.path.join(SERVER_DIR, "main.py")],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=full_env, cwd=SERVER_DIR,
        )
        self.stdout_lines = []
        self.next_id = 0
        self.request("initialize", {
            "protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "test", "version": "0"},
        })
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def send(self, message):
        self.proc.stdin.write((json.dumps(message) + "\n").encode())
        self.proc.stdin.flush()

    def request(self, method, params):
        self.next_id += 1
        self.send({"jsonrpc": "2.0", "id": self.next_id, "method": method, "params": params})
        while True:
            ready, _, _ = select.select([self.proc.stdout], [], [], 20)
            assert ready, f"no response to {method}"
            line = self.proc.stdout.readline()
            assert line, f"server exited: {self.proc.stderr.read().decode()}"
            self.stdout_lines.append(line)
            message = json.loads(line)  # anything that is not JSON on stdout fails here
            if message.get("id") == self.next_id:
                assert "error" not in message, message
                return message["result"]

    def call(self, name, **arguments):
        result = self.request("tools/call", {"name": name, "arguments": arguments})
        return result.get("isError", False), "\n".join(block["text"] for block in result["content"])

    def close(self):
        self.proc.stdin.close()
        rest = self.proc.stdout.read()
        self.proc.wait(timeout=20)
        stderr = self.proc.stderr.read().decode()
        self.proc.stdout.close()
        self.proc.stderr.close()
        self.stdout_lines.extend(rest.splitlines())
        for line in self.stdout_lines:
            assert json.loads(line)["jsonrpc"] == "2.0"
        return stderr


@pytest.fixture
def start(tmp_path, bridge):
    servers = []

    def _start(store=None, **env):
        env.setdefault("WHATSAPP_MCP_STORE_DIR", store or build_store(tmp_path / "store"))
        env.setdefault("WHATSAPP_MCP_PORT", str(bridge.server_address[1]))
        servers.append(Server(env))
        return servers[-1]

    yield _start
    for server in servers:
        if server.proc.poll() is None:
            server.proc.kill()


@pytest.mark.parametrize("env,count,send_tools", [
    ({}, 14, {"send_message", "send_poll"}),
    ({"WHATSAPP_MCP_ENABLE_SEND_MEDIA": "1"}, 16, SEND_TOOLS),
    ({"WHATSAPP_MCP_READ_ONLY": "1"}, 12, set()),
    ({"WHATSAPP_MCP_READ_ONLY": "1", "WHATSAPP_MCP_ENABLE_SEND_MEDIA": "1"}, 12, set()),
    ({"WHATSAPP_MCP_READ_ONLY": "0", "WHATSAPP_MCP_ENABLE_SEND_MEDIA": "0"}, 14, {"send_message", "send_poll"}),
])
def test_registration_modes(start, env, count, send_tools):
    server = start(**env)
    tools = server.request("tools/list", {})["tools"]
    names = {tool["name"] for tool in tools}
    assert len(tools) == count
    assert names & SEND_TOOLS == send_tools
    assert "download_media" in names
    for tool in tools:
        assert tool["description"] and tool["inputSchema"]["type"] == "object"
    if "send_message" not in names:
        is_error, text = server.call("send_message", recipient=GROUP, message="hi")
        assert is_error and "Unknown tool" in text
    server.close()


def test_every_tool_over_stdio(start, tmp_path):
    server = start(WHATSAPP_MCP_ENABLE_SEND_MEDIA="1", TZ="Africa/Johannesburg")
    document = tmp_path / "doc.pdf"
    document.write_bytes(b"%PDF")
    note = tmp_path / "note.ogg"
    note.write_bytes(b"OggS")
    calls = [
        ("search_contacts", {"query": "alice"}, "Alice Smith"),
        ("list_messages", {"chat_jid": GROUP, "after": "2026-10-09"}, "[00:30 id=G5] Alice Smith: Midnight thought"),
        ("list_chats", {}, '"name": "Family"'),
        ("get_chat", {"chat_jid": GROUP}, '"is_group": true'),
        ("get_direct_chat_by_contact", {"sender_phone_number": "+27 82 123 4567"}, ALICE),
        ("get_contact_chats", {"jid": ALICE}, GROUP),
        ("get_last_interaction", {"jid": ALICE}, "[11:06 id=G8]"),
        ("get_message_context", {"message_id": "DUP1", "chat_jid": ALICE}, "> [12:05 id=DUP1] Me: Yes, 12:30"),
        ("get_unread_messages", {"chat_jid": ALICE}, "> [09:00 id=A3] Alice Smith: Great."),
        ("list_polls", {}, '"question": "Bring what?"'),
        ("get_poll_results", {"chat_jid": "120363000000000002@g.us", "message_id": "P1"}, '"people_voted": 4'),
        ("send_message", {"recipient": ALICE, "message": "hi"}, '"success": true'),
        ("send_poll", {"recipient": ALICE, "question": "Lunch?", "options": ["12:00", "13:00"]}, '"message_id": "3EB0POLL"'),
        ("send_file", {"recipient": ALICE, "media_path": str(document)}, '"success": true'),
        ("send_audio_message", {"recipient": ALICE, "media_path": str(note)}, '"success": true'),
        ("download_media", {"message_id": "DUP1", "chat_jid": GROUP}, '"file_path": "/tmp/store/media/budget.xlsx"'),
    ]
    assert {c[0] for c in calls} == {t["name"] for t in server.request("tools/list", {})["tools"]}
    for name, arguments, expected in calls:
        is_error, text = server.call(name, **arguments)
        assert not is_error, (name, text)
        assert expected in text, (name, text)

    is_error, text = server.call("get_message_context", message_id="DUP1")
    assert is_error and "exists in several chats" in text and "Pass chat_jid" in text
    is_error, text = server.call("send_message", recipient="27820009999", message="hi")
    assert not is_error and '"message": "recipient is not an existing chat"' in text
    server.close()


def test_errors_over_stdio_keep_stdout_clean(start, tmp_path):
    server = start(store=str(tmp_path / "never-paired"), WHATSAPP_MCP_PORT="1")
    is_error, text = server.call("list_messages", chat_jid=GROUP)
    assert is_error and "message database not found" in text and "has not been run and paired yet" in text
    is_error, text = server.call("send_message", recipient=ALICE, message="hi")
    assert "not running or not paired yet" in text
    stderr = server.close()
    assert "list_messages" in stderr  # failures are logged, on stderr

    store = build_store(tmp_path / "paired")
    server = start(store=store, WHATSAPP_MCP_PORT="1")
    is_error, text = server.call("download_media", message_id="DUP1", chat_jid=GROUP)
    assert "WhatsApp bridge is not running" in text
    server.close()
