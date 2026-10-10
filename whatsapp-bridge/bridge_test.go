package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	storeDir := filepath.Join(t.TempDir(), "store")
	store, err := NewMessageStore(storeDir)
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	return &Bridge{
		cfg: Config{
			StoreDir:         storeDir,
			Port:             8080,
			SendAllowlist:    map[string]bool{},
			MaxDownloadBytes: 1 << 20,
		},
		store:  store,
		logger: waLog.Noop,
		token:  testToken,
	}
}

func (b *Bridge) testServer() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", b.guard(b.handleSend))
	mux.HandleFunc("/api/download", b.guard(b.handleDownload))
	return mux
}

// post sends a well-formed, authorised request and lets the caller break it
func post(handler http.Handler, path, body string, mutate func(r *http.Request)) (int, APIResponse) {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testToken)
	if mutate != nil {
		mutate(request)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	var response APIResponse
	json.Unmarshal(recorder.Body.Bytes(), &response)
	return recorder.Code, response
}

func TestGuardRejectsUntrustedCallers(t *testing.T) {
	bridge := newTestBridge(t)
	handler := bridge.testServer()
	body := `{"recipient":"27820000001","message":"hi"}`

	cases := []struct {
		name   string
		mutate func(r *http.Request)
		status int
	}{
		{"no token", func(r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("f", 64)) }, http.StatusUnauthorized},
		{"token without bearer", func(r *http.Request) { r.Header.Set("Authorization", testToken) }, http.StatusUnauthorized},
		{"browser origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		{"browser form content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		{"no content type", func(r *http.Request) { r.Header.Del("Content-Type") }, http.StatusUnsupportedMediaType},
		{"rebound host", func(r *http.Request) { r.Host = "evil.example:8080" }, http.StatusForbidden},
		{"other port", func(r *http.Request) { r.Host = "127.0.0.1:9999" }, http.StatusForbidden},
		{"get", func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed},
	}

	for _, c := range cases {
		for _, path := range []string{"/api/send", "/api/download"} {
			status, response := post(handler, path, body, c.mutate)
			if status != c.status {
				t.Errorf("%s %s: status %d, want %d", c.name, path, status, c.status)
			}
			if response.Success || response.Message == "" {
				t.Errorf("%s %s: want a JSON refusal with a message, got %+v", c.name, path, response)
			}
		}
	}

	// localhost is the same machine and is accepted past the guard
	status, _ := post(handler, "/api/send", `{}`, func(r *http.Request) { r.Host = "localhost:8080" })
	if status != http.StatusBadRequest {
		t.Errorf("localhost host: status %d, want the request to reach validation (400)", status)
	}
}

func TestSendRules(t *testing.T) {
	known := "27820000001@s.whatsapp.net"
	group := "120363000000000001@g.us"

	cases := []struct {
		name      string
		configure func(cfg *Config)
		body      string
		status    int
		contains  string
	}{
		{"unknown number", nil, `{"recipient":"27829999999","message":"hi"}`, http.StatusForbidden, "not an existing chat"},
		{"unknown jid", nil, `{"recipient":"27829999999@s.whatsapp.net","message":"hi"}`, http.StatusForbidden, "not an existing chat"},
		{"no recipient", nil, `{"message":"hi"}`, http.StatusBadRequest, "Recipient is required"},
		{"empty message", nil, `{"recipient":"27820000001"}`, http.StatusBadRequest, "required"},
		{"bad recipient", nil, `{"recipient":"bob","message":"hi"}`, http.StatusBadRequest, "Invalid recipient"},
		{"media disabled", nil, `{"recipient":"27820000001","media_path":"/etc/passwd"}`, http.StatusForbidden, "Sending files is disabled"},
		{"read only", func(cfg *Config) { cfg.ReadOnly = true }, `{"recipient":"27820000001","message":"hi"}`, http.StatusForbidden, "read-only"},
		{"read only beats media", func(cfg *Config) { cfg.ReadOnly = true; cfg.EnableSendMedia = true }, `{"recipient":"27820000001","media_path":"/etc/passwd"}`, http.StatusForbidden, "read-only"},
		{"not on allowlist", func(cfg *Config) { cfg.SendAllowlist[group] = true }, `{"recipient":"27820000001","message":"hi"}`, http.StatusForbidden, "allowlist"},
		// Allowed requests get as far as the WhatsApp client, which is not connected in tests
		{"known number", nil, `{"recipient":"+27 82 000 0001","message":"hi"}`, http.StatusServiceUnavailable, "Not connected"},
		{"known jid", nil, `{"recipient":"` + known + `","message":"hi"}`, http.StatusServiceUnavailable, "Not connected"},
		{"known group", nil, `{"recipient":"` + group + `","message":"hi"}`, http.StatusServiceUnavailable, "Not connected"},
		{"on allowlist", func(cfg *Config) { cfg.SendAllowlist[known] = true }, `{"recipient":"27820000001","message":"hi"}`, http.StatusServiceUnavailable, "Not connected"},
		{"media enabled", func(cfg *Config) { cfg.EnableSendMedia = true }, `{"recipient":"27820000001","media_path":"/etc/passwd"}`, http.StatusServiceUnavailable, "Not connected"},
	}

	for _, c := range cases {
		bridge := newTestBridge(t)
		now := time.Now()
		bridge.store.StoreChat(known, "Alice", now)
		bridge.store.StoreChat(group, "Family", now)
		if c.configure != nil {
			c.configure(&bridge.cfg)
		}

		status, response := post(bridge.testServer(), "/api/send", c.body, nil)
		if status != c.status || !strings.Contains(response.Message, c.contains) || response.Success {
			t.Errorf("%s: got %d %q, want %d containing %q", c.name, status, response.Message, c.status, c.contains)
		}
	}
}

func TestMediaPathStaysInsideStore(t *testing.T) {
	bridge := newTestBridge(t)
	mediaRoot, _ := filepath.Abs(filepath.Join(bridge.cfg.StoreDir, "media"))

	hostile := []struct {
		messageID, chatJID, filename, mimeType string
	}{
		{"3EB0AAAA", "27820000001@s.whatsapp.net", "../../../../Library/LaunchAgents/evil.plist", ""},
		{"../../../evil", "../../..", "invoice.pdf", "application/pdf"},
		{"/etc/cron.d/evil", "/tmp", "x", ""},
		{"..", "..", "..", ""},
		{"", "", "", ""},
		{"3EB0BBBB", "27820000001@s.whatsapp.net", "a.sh\x00.pdf", ""},
		{"3EB0CCCC", "27820000001@s.whatsapp.net", "photo.jpg/../../x", "image/jpeg"},
	}

	for _, h := range hostile {
		path, err := bridge.mediaPath(h.messageID, h.chatJID, MediaInfo{Type: "document", Filename: h.filename, MimeType: h.mimeType})
		if err != nil {
			t.Errorf("mediaPath(%q, %q, %q): %v", h.messageID, h.chatJID, h.filename, err)
			continue
		}
		relative, err := filepath.Rel(mediaRoot, path)
		if err != nil || strings.HasPrefix(relative, "..") {
			t.Errorf("mediaPath(%q, %q, %q) = %q, outside %q", h.messageID, h.chatJID, h.filename, path, mediaRoot)
		}
		if strings.Count(relative, string(filepath.Separator)) != 1 {
			t.Errorf("mediaPath(%q, %q, %q) = %q, want exactly <chat>/<file> under the media root", h.messageID, h.chatJID, h.filename, relative)
		}
		if strings.Contains(filepath.Base(path), "evil.plist") {
			t.Errorf("mediaPath used the sender's file name: %q", path)
		}
	}
}

func TestMediaExtension(t *testing.T) {
	cases := []struct {
		media MediaInfo
		want  string
	}{
		{MediaInfo{Type: "image", MimeType: "image/png"}, ".png"},
		{MediaInfo{Type: "audio", MimeType: "audio/ogg; codecs=opus"}, ".ogg"},
		{MediaInfo{Type: "document", Filename: "Invoice 42.XLSX", MimeType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}, ".xlsx"},
		{MediaInfo{Type: "document", Filename: "evil.plist/../x"}, ".bin"},
		{MediaInfo{Type: "document", Filename: "noextension"}, ".bin"},
		{MediaInfo{Type: "document", Filename: "a.b c"}, ".bin"},
		{MediaInfo{Type: "video"}, ".mp4"},
	}
	for _, c := range cases {
		if got := mediaExtension(c.media); got != c.want {
			t.Errorf("mediaExtension(%+v) = %q, want %q", c.media, got, c.want)
		}
	}
}

func TestDownloadRules(t *testing.T) {
	bridge := newTestBridge(t)
	handler := bridge.testServer()
	chat := "27820000001@s.whatsapp.net"
	now := time.Now()
	bridge.store.StoreChat(chat, "Alice", now)

	keys := MediaInfo{DirectPath: "/v/t62/x.enc", MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}}
	store := func(id, content string, media MediaInfo) {
		t.Helper()
		err := bridge.store.StoreMessage(StoredMessage{ID: id, ChatJID: chat, Sender: "27820000001", Content: content, Timestamp: now, Media: media})
		if err != nil {
			t.Fatalf("StoreMessage: %v", err)
		}
	}

	text := MediaInfo{}
	store("TEXT", "hello", text)

	huge := keys
	huge.Type, huge.FileLength = "video", 5<<20
	store("HUGE", "", huge)

	hostile := keys
	hostile.Type, hostile.Filename, hostile.FileLength = "document", "../../../../evil.plist", 10
	store("HOSTILE", "", hostile)

	request := func(id string) (int, APIResponse) {
		return post(handler, "/api/download", `{"message_id":"`+id+`","chat_jid":"`+chat+`"}`, nil)
	}

	if status, response := request("MISSING"); status != http.StatusNotFound {
		t.Errorf("missing message: %d %q", status, response.Message)
	}
	if status, response := request("TEXT"); status != http.StatusBadRequest {
		t.Errorf("text message: %d %q", status, response.Message)
	}
	if status, response := request("HUGE"); status != http.StatusRequestEntityTooLarge || !strings.Contains(response.Message, "limit") {
		t.Errorf("oversized media: %d %q", status, response.Message)
	}
	if status, response := request("HOSTILE"); status != http.StatusServiceUnavailable {
		t.Errorf("download without a connection: %d %q", status, response.Message)
	}

	// Media that is already on disk is served from the path built from the message ID
	path, err := bridge.mediaPath("HOSTILE", chat, hostile)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte("0123456789"), 0600)

	status, response := request("HOSTILE")
	if status != http.StatusOK || !response.Success || response.Path != path {
		t.Fatalf("cached media: %d %+v, want path %q", status, response, path)
	}
	if response.Filename != hostile.Filename {
		t.Errorf("display name = %q, want the sender's name %q", response.Filename, hostile.Filename)
	}
	mediaRoot, _ := filepath.Abs(filepath.Join(bridge.cfg.StoreDir, "media"))
	if !strings.HasPrefix(response.Path, mediaRoot+string(filepath.Separator)) {
		t.Errorf("path %q is outside %q", response.Path, mediaRoot)
	}
}

func TestStorePermissionsAndToken(t *testing.T) {
	bridge := newTestBridge(t)
	storeDir := bridge.cfg.StoreDir

	token, err := loadOrCreateToken(storeDir)
	if err != nil || len(token) != 64 {
		t.Fatalf("loadOrCreateToken = %q, %v", token, err)
	}
	again, _ := loadOrCreateToken(storeDir)
	if again != token {
		t.Errorf("token changed between starts")
	}

	restrictFiles(storeDir)
	if info, _ := os.Stat(storeDir); info.Mode().Perm() != 0700 {
		t.Errorf("store directory mode %o, want 700", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(storeDir)
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Mode().IsRegular() && info.Mode().Perm() != 0600 {
			t.Errorf("%s mode %o, want 600", entry.Name(), info.Mode().Perm())
		}
	}
}

func TestStoreChatAndTimestamps(t *testing.T) {
	bridge := newTestBridge(t)
	store := bridge.store
	chat := "27820000001@s.whatsapp.net"
	johannesburg := time.FixedZone("SAST", 2*3600)
	newer := time.Date(2026, 10, 10, 17, 4, 5, 0, johannesburg)
	older := newer.Add(-48 * time.Hour)

	store.StoreChat(chat, "27820000001", newer)
	store.StoreChat(chat, "ignored", older) // history arriving after live messages must not move the chat back

	var name, last string
	store.db.QueryRow("SELECT name, CAST(last_message_time AS TEXT) FROM chats WHERE jid = ?", chat).Scan(&name, &last)
	if last != "2026-10-10 15:04:05+00:00" {
		t.Errorf("last_message_time = %q, want UTC 2026-10-10 15:04:05+00:00", last)
	}
	if name != "27820000001" {
		t.Errorf("name = %q, want it unchanged by a later StoreChat", name)
	}

	store.SetChatName(chat, "Alice")
	store.SetChatName(chat, "") // an unknown name never erases a known one
	if name, _ := store.ChatName(chat); name != "Alice" {
		t.Errorf("name = %q, want Alice", name)
	}
	store.SetChatName("nobody@s.whatsapp.net", "Ghost")
	if _, exists := store.ChatName("nobody@s.whatsapp.net"); exists {
		t.Errorf("SetChatName created a chat")
	}

	store.StoreMessage(StoredMessage{ID: "A", ChatJID: chat, Sender: "27820000001", Content: "first", Timestamp: newer})
	store.UpdateMessageContent("A", chat, "edited")
	var content, timestamp string
	store.db.QueryRow("SELECT content, CAST(timestamp AS TEXT) FROM messages WHERE id = 'A'").Scan(&content, &timestamp)
	if content != "edited" || timestamp != "2026-10-10 15:04:05+00:00" {
		t.Errorf("message = %q at %q", content, timestamp)
	}
	store.DeleteMessage("A", chat)
	var count int
	store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if count != 0 {
		t.Errorf("deleted message still stored")
	}
}

func TestMergeChat(t *testing.T) {
	bridge := newTestBridge(t)
	store := bridge.store
	hidden := "111222333444555@lid"
	phone := "27820000001@s.whatsapp.net"
	early := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	late := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)

	store.StoreChat(phone, "Alice", early)
	store.StoreMessage(StoredMessage{ID: "SHARED", ChatJID: phone, Sender: "27820000001", Content: "kept", Timestamp: early})
	store.StoreChat(hidden, "111222333444555", late)
	store.StoreMessage(StoredMessage{ID: "SHARED", ChatJID: hidden, Sender: hidden, Content: "duplicate", Timestamp: early})
	store.StoreMessage(StoredMessage{ID: "NEW", ChatJID: hidden, Sender: hidden, Content: "moved", Timestamp: late})

	if err := store.MergeChat(hidden, phone, "27820000001"); err != nil {
		t.Fatalf("MergeChat: %v", err)
	}
	store.RenameSender(hidden, "27820000001")

	if chats, _ := store.HiddenChats(); len(chats) != 0 {
		t.Errorf("hidden chat still present: %v", chats)
	}
	if senders, _ := store.HiddenSenders(); len(senders) != 0 {
		t.Errorf("hidden senders still present: %v", senders)
	}

	var count int
	var last string
	store.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid = ?", phone).Scan(&count)
	store.db.QueryRow("SELECT CAST(last_message_time AS TEXT) FROM chats WHERE jid = ?", phone).Scan(&last)
	if count != 2 || last != formatTimestamp(late) {
		t.Errorf("merged chat has %d messages, last %q; want 2 and %q", count, last, formatTimestamp(late))
	}
	if name, _ := store.ChatName(phone); name != "Alice" {
		t.Errorf("merge changed the name to %q", name)
	}

	// Merging into a chat that does not exist yet creates it
	other := "999888777666555@lid"
	store.StoreChat(other, "999888777666555", late)
	store.StoreMessage(StoredMessage{ID: "ONLY", ChatJID: other, Sender: other, Content: "hello", Timestamp: late})
	if err := store.MergeChat(other, "27820000002@s.whatsapp.net", "27820000002"); err != nil {
		t.Fatalf("MergeChat into new chat: %v", err)
	}
	if name, exists := store.ChatName("27820000002@s.whatsapp.net"); !exists || name != "27820000002" {
		t.Errorf("new chat = %q, %v", name, exists)
	}
}

func TestNormalizeRecipient(t *testing.T) {
	valid := map[string]string{
		"27820000001":                   "27820000001@s.whatsapp.net",
		"+27 82 000-0001":               "27820000001@s.whatsapp.net",
		"27820000001@s.whatsapp.net":    "27820000001@s.whatsapp.net",
		"27820000001:12@s.whatsapp.net": "27820000001@s.whatsapp.net",
		"120363000000000001@g.us":       "120363000000000001@g.us",
	}
	for input, want := range valid {
		jid, err := normalizeRecipient(input)
		if err != nil || jid.String() != want {
			t.Errorf("normalizeRecipient(%q) = %q, %v; want %q", input, jid.String(), err, want)
		}
	}
	for _, input := range []string{"", "bob", "123", "2782000000a", "@g.us", "../../x"} {
		if jid, err := normalizeRecipient(input); err == nil {
			t.Errorf("normalizeRecipient(%q) = %q, want an error", input, jid.String())
		}
	}
}

func TestAnalyzeOggOpus(t *testing.T) {
	// Garbage and truncated input must be refused or survived, never panic
	for _, data := range [][]byte{nil, []byte("Ogg"), []byte("OggS"), append([]byte("OggS"), make([]byte, 40)...), append([]byte("OggS"), 0xff)} {
		analyzeOggOpus(data)
	}
	truncated := append([]byte("OggS"), make([]byte, 23)...)
	truncated[26] = 1
	truncated = append(truncated, 200, 'O', 'p', 'u', 's', 'H', 'e', 'a', 'd')
	analyzeOggOpus(truncated)
}

func TestServerListensOnLoopbackOnly(t *testing.T) {
	bridge := newTestBridge(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bridge.cfg.Port = probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	if err := bridge.startRESTServer(); err != nil {
		t.Fatalf("startRESTServer: %v", err)
	}

	// A real request over the loopback interface gets through the guard
	url := fmt.Sprintf("http://127.0.0.1:%d/api/send", bridge.cfg.Port)
	request, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("loopback request failed: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("loopback request: status %d, want 400", response.StatusCode)
	}

	// The same port is closed on every other address of this machine
	addresses, _ := net.InterfaceAddrs()
	checked := 0
	for _, address := range addresses {
		ipNet, ok := address.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		checked++
		target := net.JoinHostPort(ipNet.IP.String(), fmt.Sprint(bridge.cfg.Port))
		if conn, err := net.DialTimeout("tcp", target, time.Second); err == nil {
			conn.Close()
			t.Errorf("API is reachable on %s", target)
		}
	}
	t.Logf("checked %d non-loopback addresses", checked)
}
