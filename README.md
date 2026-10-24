# 💬 WhatsApp MCP

**Your WhatsApp, with Claude on your side.**

You come back from a weekend away. There are 187 unread messages in the family group, someone started a poll about the braai, and a friend asked you something on Thursday that you never got back to. Instead of scrolling for twenty minutes, you ask:

> *"What did I miss in the family group, and is there anything I need to answer?"*

This is a [Model Context Protocol](https://modelcontextprotocol.io) server that connects Claude (or Cursor, or any MCP client) to your **personal WhatsApp account**. It can read and search your chats, tell you what's unread, count poll votes, and, when you say so, send messages and polls for you.

Everything is stored on your own machine. Messages only reach the model when Claude calls a tool to read them, and your client asks you before those calls.

![WhatsApp MCP](./example-use.png)

> ⚠️ **Read this first:** like any MCP server that reads private data and can send messages, this one is exposed to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). A cleverly written message could try to trick the model into leaking your chats (prompt injection). The [Security](#-security) section explains the defences, and which ones are up to you.

---

## ✨ What it can do

| | |
|---|---|
| 📬 **Catch you up** | Knows how far you've read each chat on your phone, so *"what did I miss?"* means exactly that. No guessing at time ranges. |
| 🔍 **Find that thing someone said** | Searches every chat, shows matches in context, and handles names, numbers and dates in your time zone. |
| ✍️ **Draft and send replies** | Writes in context and sends to people or groups, but only to chats you already have. |
| 📊 **Run polls** | Sends polls, shows them in chats with live counts, and tells you who voted for what. |
| 🖼️ **Handle media** | Sees photos, videos, voice notes and documents in chats, downloads them on request, and can send files if you switch that on. |
| 🔒 **Stay private** | The bridge only listens on your machine and needs a token. Read-only mode and a send allowlist are available. A git check stops your data from being committed. |

### Things to try

- *"Summarise my unread chats, most important first."*
- *"What did Alice say about the budget last week?"*
- *"What's winning in the outreach poll, and have I voted yet?"*
- *"Draft a friendly reply to Bob's last message, but don't send it until I've read it."*
- *"Send a poll to the team: Tuesday or Thursday for the next meeting?"*
- *"Did anyone send me a document this week?"*

---

## 🚀 Getting started

### You'll need

- **Go**, for the WhatsApp bridge
- **[uv](https://docs.astral.sh/uv/)**, which installs the right Python for you: `curl -LsSf https://astral.sh/uv/install.sh | sh`
- **Claude Desktop** or **Cursor**
- **FFmpeg** (optional), only needed to turn audio files into playable voice notes

### 1. Clone it

```bash
git clone https://github.com/ruanspies/whatsapp-mcp.git
cd whatsapp-mcp
git config core.hooksPath .githooks   # turns on the private-data check, see below
```

### 2. Start the bridge and link your phone

```bash
cd whatsapp-bridge
go run .
```

A QR code appears in the terminal. On your phone, open **WhatsApp → Linked Devices → Link a device** and scan it. Your recent history then syncs, which can take a few minutes for a busy account. Wait for `History sync complete`.

🍏 **On a Mac** you can double-click `whatsapp-bridge/start-bridge.command` instead. It rebuilds the bridge whenever the code has changed, and won't start a second copy if one is already running. Point a Dock app at it to start your bridge with one click.

> Always start the bridge from inside `whatsapp-bridge`. It keeps its data in `./store`, and that's where the MCP server looks. Leave it running while you use Claude. WhatsApp may ask you to link it again after about 20 days.

### 3. Tell Claude about it

Add this to your client's MCP config, filling in the two paths:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/path/to/uv",
      "args": ["--directory", "/path/to/whatsapp-mcp/whatsapp-mcp-server", "run", "main.py"]
    }
  }
}
```

- `which uv` gives you the first path.
- `pwd` inside the repo, plus `/whatsapp-mcp-server`, gives you the second.

| Client | Config file |
|---|---|
| Claude Desktop (macOS) | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Cursor | `~/.cursor/mcp.json` |

### 4. Restart your client

Quit and reopen Claude Desktop (or restart Cursor), and WhatsApp shows up among your tools. Say hi to your group chats. 👋

<details>
<summary><b>🪟 Running on Windows?</b></summary>

`go-sqlite3` needs CGO, which is off by default on Windows.

1. Install a C compiler. [MSYS2](https://www.msys2.org/) works well; add its `ucrt64\bin` folder to your `PATH` ([step-by-step guide](https://code.visualstudio.com/docs/cpp/config-mingw)).
2. Enable CGO and run:

   ```bash
   cd whatsapp-bridge
   go env -w CGO_ENABLED=1
   go run .
   ```

Without this you'll see `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`
</details>

---

## 🧰 The toolbox

**Reading and finding**

| Tool | What it's for |
|---|---|
| `list_messages` | Messages as a readable transcript, filtered by chat, person, text or time, with context around search hits |
| `get_message_context` | The conversation around one message |
| `get_last_interaction` | Your latest exchange with someone |
| `search_contacts` | Find people by name or number |
| `list_chats` | Your chats, with unread counts. `unread_only=True` lists just the ones that need you |
| `get_chat` · `get_direct_chat_by_contact` · `get_contact_chats` | Look up a chat, someone's direct chat, or every chat a person is in |
| `get_unread_messages` | Everything you haven't read, plus a few earlier lines for context: one chat, or every unread chat at once (`chat_type="groups"` for just groups) |

**Polls** 📊

| Tool | What it's for |
|---|---|
| `list_polls` | Recent polls: how many voted, what's leading, and whether *you've* voted |
| `get_poll_results` | Each option with its count and the names of who picked it |
| `send_poll` | Send a choose-one or choose-many poll (2–12 options) |

**Sending and media**

| Tool | What it's for |
|---|---|
| `send_message` | Send a text to a person or group |
| `download_media` | Save a photo, video, voice note or document locally and get its path |
| `send_file` · `send_audio_message` | Send files and voice notes, **off by default** (see [Configuration](#configuration)) |

Here's what Claude sees when it reads a chat:

```text
Chat: Family (120363000000000001@g.us, group)
-- Sat 2026-10-17 UTC+02:00 --
[09:14 id=3EB0A1] Alice: Who's bringing the braai wood? 🔥
[09:15 id=3EB0A2] Bob: [image: woodpile.jpg] Sorted
> [09:20 id=3EB0A3] Alice: 📊 Poll: Start time? (choose one)
    • 12:00: 3
    • 13:00: 1
    (4 voted)
```

Times are shown in your time zone, polls carry their live counts, and lines starting with `> ` are search hits or unread messages.

---

## 📬 Catching up: read and unread

The bridge keeps track of **how far you've read every chat on any of your devices**, so Claude can tell you exactly what you missed. Reading through Claude never marks anything as read on your phone, so your friends' blue ticks aren't affected. 🤫

**Where it comes from**
- The read notices your phone sends your other linked devices.
- "Mark as read" and "mark as unread" from your chat list.
- Your own replies, because writing in a chat means you've read what came before.
- When you first link the bridge, WhatsApp also sends every chat's unread count.

**Good to know**
- **Bridge off for a while?** WhatsApp holds the read notices and delivers them when the bridge reconnects.
- **`unread_count` is `null`** means the read point isn't known yet. It becomes known the next time you read or reply in that chat.
- **Counts may be slightly off.** Times are stored to the second, so a message sent in the same second as the last one you read counts as read. Your phone also counts some system messages.
- **Chat settings out of step?** The bridge fixes this by itself. If its copy of your chat settings drifts from WhatsApp's (you'll see "mismatching LTHash" in the log), it downloads them again from scratch.

## 📊 Polls

Polls show up in transcripts with live counts, and Claude can list them, break down results by voter, and send new ones.

- **Live votes** are decrypted as they arrive. If someone changes or withdraws their vote, the count follows.
- **Older polls** come in with the history WhatsApp sends when you link the bridge, votes included.
- **Claude never votes for you.** Sending polls follows the same rules as messages: existing chats only, the allowlist, and nothing at all in read-only mode.
- **Limits:** options added after a poll was sent are counted as "an option added after the poll was sent", and edits to a poll aren't picked up. Polls from before you set up this version only appear after you [re-link](#-re-linking-the-bridge).

## 🖼️ Media

Photos, videos, voice notes and documents show up in transcripts as markers like `[image: photo.jpg]`. Claude can fetch the file with `download_media`, which saves it under `whatsapp-bridge/store/media/` under a safe name based on the message ID.

Sending files is **off by default**, because once it's on the bridge will send any file your user account can read (except its own store). If you want it, start both the bridge and the MCP server with `WHATSAPP_MCP_ENABLE_SEND_MEDIA=1`:

- `send_file` handles images, videos and documents.
- `send_audio_message` sends playable voice notes. It wants `.ogg` Opus, and converts anything else if FFmpeg is installed.

---

## 🔒 Security

The bridge exposes a small REST API that only the MCP server should use. It's protected like this:

- 🏠 **Local only:** it listens on `127.0.0.1`, so other devices on your network can't reach it.
- 🔑 **Token required:** every request must present the token in `whatsapp-bridge/store/api_token`, which the bridge creates on first start.
- 🌐 **No browsers:** requests from web pages are refused (the `Origin`, `Host` and `Content-Type` checks).
- 📇 **Existing chats only:** Claude can't message a new number. Note that anyone who has ever messaged you or added you to a group counts as existing. For tighter control, set `WHATSAPP_MCP_SEND_ALLOWLIST`.
- 📁 **Careful with files:** sending files is off unless you enable it, and downloads never use the file name the sender chose.
- ✏️ **Only the sender can edit:** only a message's sender can edit or delete the stored copy, and only a poll's sender can change its options.
- 🧾 **No fake lines:** in transcripts, a line that isn't indented is always a real message. Text inside a message, a file name or a poll option can't pass itself off as one.

**The part that's up to you:** messages are written by other people, and the model reads them. A message can try to instruct the model, so keep your client's approval prompt switched on for `send_message` and `send_poll`, and treat downloaded files as untrusted. If you only want Claude to read, set `WHATSAPP_MCP_READ_ONLY=1`.

Your session keys live in `whatsapp-bridge/store/whatsapp.db`. **Anyone with that file can act as your WhatsApp account.** The store directory is readable by your user only. Never share it, and never commit it.

### 🛡️ Keeping your private data out of git

The store is in `.gitignore`. As a second line of defence, `scripts/check-private.sh` blocks any commit or push that contains:

- the store, a database, or a token or key
- on a machine with a paired bridge, any real phone number or group ID from your account

Turn it on once per clone with `git config core.hooksPath .githooks`. The same check runs on GitHub for every push. If it flags a line by mistake, end that line with `private-check: allow`.

## Configuration

Set these for the bridge. Those marked **both** must also go in the `env` block of the MCP server's entry in your client config.

| Variable | Default | For | What it does |
|---|---|---|---|
| `WHATSAPP_MCP_READ_ONLY` | off | both | `1` turns off all sending: messages, polls and files |
| `WHATSAPP_MCP_ENABLE_SEND_MEDIA` | off | both | `1` enables `send_file` and `send_audio_message` |
| `WHATSAPP_MCP_SEND_ALLOWLIST` | empty | bridge | Comma-separated numbers or JIDs. When set, messages and polls can only go to these chats |
| `WHATSAPP_MCP_MAX_DOWNLOAD_MB` | 100 | bridge | Largest media file that will be downloaded |
| `WHATSAPP_MCP_PORT` | 8080 | both | Port of the local REST API |
| `WHATSAPP_MCP_STORE_DIR` | `whatsapp-bridge/store` | both | Where the databases, token and media live |

---

## 🏗️ How it fits together

```text
 ┌──────────┐   MCP    ┌──────────────┐  reads   ┌──────────────┐
 │  Claude  │ ───────▶ │  MCP server  │ ───────▶ │  SQLite      │
 │ (client) │ ◀─────── │   (Python)   │          │  messages.db │
 └──────────┘          └──────┬───────┘          └──────▲───────┘
                              │ send / download         │ writes
                              ▼ (localhost + token)     │
                       ┌──────────────┐  whatsmeow  ┌───┴─────────┐
                       │    Bridge    │ ◀─────────▶ │  WhatsApp   │
                       │     (Go)     │             │  servers    │
                       └──────────────┘             └─────────────┘
```

- **The bridge** (`whatsapp-bridge/`, Go) links to WhatsApp as a companion device using [whatsmeow](https://github.com/tulir/whatsmeow). It writes everything it receives to `messages.db`: messages, media details, read state, polls and votes. It also serves a small local API for sending and downloading.
- **The MCP server** (`whatsapp-mcp-server/`, Python) opens that database **read-only** to answer questions, and asks the bridge whenever something needs to be sent or downloaded.

## 🔄 Re-linking the bridge

A fresh link makes WhatsApp resend recent history, exact unread counts, and polls with their votes. Your existing messages stay.

1. Stop the bridge with Ctrl+C.
2. Move the session file out of the way: `mv whatsapp-bridge/store/whatsapp.db whatsapp-bridge/store/whatsapp.db.bak`. Keep `messages.db` and `api_token`.
3. On your phone, remove the old device under **Linked Devices**.
4. Start the bridge, scan the new QR code, and wait for `History sync complete`.
5. Once everything looks right, delete the backup.

## 🩺 Troubleshooting

| Symptom | Fix |
|---|---|
| A tool says *"WhatsApp bridge is not running"* | Start it (`go run .` in `whatsapp-bridge`, or `start-bridge.command`) and leave it running |
| No QR code appears | Restart the bridge, and check that your terminal can draw the block characters |
| *"Device limit reached"* | Remove an old device under **Linked Devices** on your phone |
| History is empty right after linking | Give it a few minutes; big accounts take a while to sync |
| *"Client outdated (405)"* | WhatsApp no longer accepts this whatsmeow version. Run `go get go.mau.fi/whatsmeow@latest && go mod tidy` in `whatsapp-bridge` |
| *"mismatching LTHash"* in the log | Normally fixed automatically: look for "downloading them again" right after. If the warning keeps coming back, [re-link](#-re-linking-the-bridge) |
| *"no sender key"* or *"didn't find app state key"* | Harmless; the bridge asks your phone for the missing key and carries on |
| New tools don't appear in Claude | Quit and reopen Claude Desktop so it reloads the server |
| Everything is badly out of sync | Stop the bridge, delete `messages.db` and `whatsapp.db` in the store, and link again from scratch |
| uv permission errors | Use the full path to `uv` in your client config |

For Claude Desktop logs and integration issues, see the [MCP docs](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues).

---

## 🙏 Credits

Built on the original [WhatsApp MCP](https://github.com/lharries/whatsapp-mcp) by Luke Harries, and powered by [whatsmeow](https://github.com/tulir/whatsmeow). This fork adds:

- hardened security
- read and unread tracking
- polls
- better transcripts and search
- a lot of tests

Licensed under the [MIT License](./LICENSE).
