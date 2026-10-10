package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
)

// APIResponse is the body of every API response. Filename and Path are only set by downloads.
type APIResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// apiError is a refusal or failure with the HTTP status it should be reported as
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string {
	return e.message
}

func refuse(status int, format string, args ...interface{}) *apiError {
	return &apiError{status: status, message: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, response APIResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

func writeError(w http.ResponseWriter, err *apiError) {
	writeJSON(w, err.status, APIResponse{Success: false, Message: err.message})
}

// loadOrCreateToken returns the shared secret that callers of the API must present.
// It lives in the store directory, readable by the owner only.
func loadOrCreateToken(storeDir string) (string, error) {
	tokenPath := filepath.Join(storeDir, "api_token")

	if data, err := os.ReadFile(tokenPath); err == nil {
		if token := strings.TrimSpace(string(data)); len(token) >= 32 {
			if err := os.Chmod(tokenPath, 0600); err != nil {
				return "", fmt.Errorf("failed to restrict API token: %v", err)
			}
			return token, nil
		}
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("failed to generate API token: %v", err)
	}
	token := hex.EncodeToString(secret)
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0600); err != nil {
		return "", fmt.Errorf("failed to save API token: %v", err)
	}
	return token, nil
}

// guard rejects every request that does not come from a local program holding the token.
// The Host, Origin and Content-Type checks keep web pages in a browser from reaching the API.
func (b *Bridge) guard(next func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	allowedHosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", b.cfg.Port): true,
		fmt.Sprintf("localhost:%d", b.cfg.Port): true,
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			writeError(w, refuse(http.StatusMethodNotAllowed, "Method not allowed"))
			return
		}

		if !allowedHosts[strings.ToLower(r.Host)] {
			writeError(w, refuse(http.StatusForbidden, "Requests must be addressed to 127.0.0.1"))
			return
		}

		if r.Header.Get("Origin") != "" {
			writeError(w, refuse(http.StatusForbidden, "Requests from web pages are not allowed"))
			return
		}

		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			writeError(w, refuse(http.StatusUnsupportedMediaType, "Content-Type must be application/json"))
			return
		}

		presented, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !hasBearer || subtle.ConstantTimeCompare([]byte(presented), []byte(b.token)) != 1 {
			writeError(w, refuse(http.StatusUnauthorized, "Missing or invalid API token"))
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next(w, r)
	}
}

// checkSendAllowed applies the sending rules and returns the chat to send to
func (b *Bridge) checkSendAllowed(req SendMessageRequest) (string, *apiError) {
	if b.cfg.ReadOnly {
		return "", refuse(http.StatusForbidden, "Sending is disabled: the bridge is running in read-only mode")
	}

	// Validate request
	if strings.TrimSpace(req.Recipient) == "" {
		return "", refuse(http.StatusBadRequest, "Recipient is required")
	}

	if req.Message == "" && req.MediaPath == "" {
		return "", refuse(http.StatusBadRequest, "Message or media path is required")
	}

	if req.MediaPath != "" && !b.cfg.EnableSendMedia {
		return "", refuse(http.StatusForbidden, "Sending files is disabled. Set WHATSAPP_MCP_ENABLE_SEND_MEDIA=1 for the bridge and the MCP server to enable it")
	}

	recipient, err := normalizeRecipient(req.Recipient)
	if err != nil {
		return "", refuse(http.StatusBadRequest, "Invalid recipient: %v", err)
	}
	chatJID := recipient.String()

	// Only chats that already exist can be written to, so a message can never go to a stranger's number
	if _, exists := b.store.ChatName(chatJID); !exists {
		return "", refuse(http.StatusForbidden, "Recipient %s is not an existing chat. Messages can only be sent to chats already in your history", chatJID)
	}

	if len(b.cfg.SendAllowlist) > 0 && !b.cfg.SendAllowlist[chatJID] {
		return "", refuse(http.StatusForbidden, "Recipient %s is not on the send allowlist", chatJID)
	}

	return chatJID, nil
}

// Handler for sending messages
func (b *Bridge) handleSend(w http.ResponseWriter, r *http.Request) {
	// Parse the request body
	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, refuse(http.StatusBadRequest, "Invalid request format"))
		return
	}

	chatJID, refusal := b.checkSendAllowed(req)
	if refusal != nil {
		writeError(w, refusal)
		return
	}

	// Send the message
	if refusal := b.sendWhatsAppMessage(r.Context(), chatJID, req.Message, req.MediaPath); refusal != nil {
		writeError(w, refusal)
		return
	}

	writeJSON(w, http.StatusOK, APIResponse{Success: true, Message: fmt.Sprintf("Message sent to %s", chatJID)})
}

// Handler for downloading media
func (b *Bridge) handleDownload(w http.ResponseWriter, r *http.Request) {
	// Parse the request body
	var req DownloadMediaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, refuse(http.StatusBadRequest, "Invalid request format"))
		return
	}

	// Validate request
	if req.MessageID == "" || req.ChatJID == "" {
		writeError(w, refuse(http.StatusBadRequest, "Message ID and Chat JID are required"))
		return
	}

	// Download the media
	media, path, refusal := b.downloadMedia(r.Context(), req.MessageID, req.ChatJID)
	if refusal != nil {
		writeError(w, refusal)
		return
	}

	filename := media.Filename
	if filename == "" {
		filename = filepath.Base(path)
	}

	writeJSON(w, http.StatusOK, APIResponse{
		Success:  true,
		Message:  fmt.Sprintf("Successfully downloaded %s media", media.Type),
		Filename: filename,
		Path:     path,
	})
}

// safeName reduces text that came from WhatsApp to characters that are harmless in a file name
func safeName(name string) string {
	var safe strings.Builder
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			safe.WriteRune(c)
		default:
			safe.WriteRune('_')
		}
		if safe.Len() >= 120 {
			break
		}
	}
	if safe.Len() == 0 {
		return "_"
	}
	return safe.String()
}

var mimeExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
	"video/3gpp":      ".3gp",
	"audio/ogg":       ".ogg",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"audio/aac":       ".aac",
	"application/pdf": ".pdf",
}

var defaultExtensions = map[string]string{
	"image":    ".jpg",
	"video":    ".mp4",
	"audio":    ".ogg",
	"document": ".bin",
}

func isAlphanumeric(text string) bool {
	for _, c := range text {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// mediaExtension picks the extension of a downloaded file. The sender's file name
// only contributes its extension, and only when that is short and alphanumeric.
func mediaExtension(media MediaInfo) string {
	mimeType, _, _ := mime.ParseMediaType(media.MimeType)
	if ext, ok := mimeExtensions[mimeType]; ok {
		return ext
	}

	ext := strings.ToLower(filepath.Ext(media.Filename))
	if len(ext) >= 2 && len(ext) <= 9 && isAlphanumeric(ext[1:]) {
		return ext
	}

	return defaultExtensions[media.Type]
}

// mediaPath returns where the media of a message is saved. The location is built
// from the message ID, never from the file name the sender chose, and is always
// inside the media directory of the store.
func (b *Bridge) mediaPath(messageID, chatJID string, media MediaInfo) (string, error) {
	mediaRoot, err := filepath.Abs(filepath.Join(b.cfg.StoreDir, "media"))
	if err != nil {
		return "", err
	}

	path := filepath.Join(mediaRoot, safeName(chatJID), safeName(messageID)+mediaExtension(media))

	relative, err := filepath.Rel(mediaRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("media path escapes the store")
	}
	return path, nil
}

// cappedFile is a download target that refuses to grow past a limit. It wraps the
// file rather than embedding it so that every write goes through the check.
type cappedFile struct {
	file     *os.File
	limit    int64
	exceeded bool
}

var errMediaTooLarge = errors.New("media is over the download limit")

func (c *cappedFile) Write(p []byte) (int, error) {
	offset, err := c.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if offset+int64(len(p)) > c.limit {
		c.exceeded = true
		return 0, errMediaTooLarge
	}
	return c.file.Write(p)
}

func (c *cappedFile) WriteAt(p []byte, offset int64) (int, error) {
	if offset+int64(len(p)) > c.limit {
		c.exceeded = true
		return 0, errMediaTooLarge
	}
	return c.file.WriteAt(p, offset)
}

func (c *cappedFile) Truncate(size int64) error {
	if size > c.limit {
		c.exceeded = true
		return errMediaTooLarge
	}
	return c.file.Truncate(size)
}

func (c *cappedFile) Read(p []byte) (int, error)                { return c.file.Read(p) }
func (c *cappedFile) ReadAt(p []byte, off int64) (int, error)   { return c.file.ReadAt(p, off) }
func (c *cappedFile) Seek(off int64, whence int) (int64, error) { return c.file.Seek(off, whence) }
func (c *cappedFile) Stat() (os.FileInfo, error)                { return c.file.Stat() }

// Extract direct path from a WhatsApp media URL. Only messages stored by an
// older version of the bridge need this; newer ones carry the direct path.
func extractDirectPathFromURL(url string) string {
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...
	// The direct path is everything after the domain, query included
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 || !strings.Contains(parts[1], "?") {
		return ""
	}
	return "/" + parts[1]
}

// Function to download media from a message
func (b *Bridge) downloadMedia(ctx context.Context, messageID, chatJID string) (MediaInfo, string, *apiError) {
	// Get media info from the database
	media, err := b.store.GetMediaInfo(messageID, chatJID)
	if errors.Is(err, sql.ErrNoRows) {
		return media, "", refuse(http.StatusNotFound, "Message %s not found in chat %s", messageID, chatJID)
	} else if err != nil {
		return media, "", refuse(http.StatusInternalServerError, "Failed to look up message: %v", err)
	}

	// Check if this is a media message
	var waMediaType whatsmeow.MediaType
	switch media.Type {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return media, "", refuse(http.StatusBadRequest, "Message %s has no downloadable media", messageID)
	}

	localPath, err := b.mediaPath(messageID, chatJID, media)
	if err != nil {
		return media, "", refuse(http.StatusInternalServerError, "Failed to choose a location for the media: %v", err)
	}

	// Check if file already exists
	if _, err := os.Stat(localPath); err == nil {
		return media, localPath, nil
	}

	if media.FileLength > uint64(b.cfg.MaxDownloadBytes) {
		return media, "", refuse(http.StatusRequestEntityTooLarge, "Media is %d MB, over the %d MB download limit (WHATSAPP_MCP_MAX_DOWNLOAD_MB)",
			media.FileLength>>20, b.cfg.MaxDownloadBytes>>20)
	}

	directPath := media.DirectPath
	if directPath == "" && media.URL != "" {
		directPath = extractDirectPathFromURL(media.URL)
	}

	// If we don't have all the media info we need, we can't download
	if directPath == "" || len(media.MediaKey) == 0 || len(media.FileEncSHA256) == 0 {
		return media, "", refuse(http.StatusBadRequest, "Incomplete media information for download")
	}

	if !b.client.IsConnected() {
		return media, "", refuse(http.StatusServiceUnavailable, "Not connected to WhatsApp")
	}

	// Download straight to a file, so the size limit holds whatever length the sender declared
	if err := os.MkdirAll(filepath.Dir(localPath), 0700); err != nil {
		return media, "", refuse(http.StatusInternalServerError, "Failed to create media directory: %v", err)
	}
	partial, err := os.CreateTemp(filepath.Dir(localPath), ".download-*")
	if err != nil {
		return media, "", refuse(http.StatusInternalServerError, "Failed to save media file: %v", err)
	}
	defer os.Remove(partial.Name())
	defer partial.Close()

	ctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	// The encrypted file is slightly longer than the media itself
	target := &cappedFile{file: partial, limit: b.cfg.MaxDownloadBytes + 1<<16}
	err = b.client.DownloadMediaWithPathToFile(ctx, directPath, media.FileEncSHA256, media.FileSHA256, media.MediaKey, waMediaType, "", len(media.FileSHA256) == 0, target)
	if target.exceeded {
		return media, "", refuse(http.StatusRequestEntityTooLarge, "Media is over the %d MB download limit (WHATSAPP_MCP_MAX_DOWNLOAD_MB)", b.cfg.MaxDownloadBytes>>20)
	}
	if err != nil {
		return media, "", refuse(http.StatusInternalServerError, "Failed to download media (old media may no longer be on WhatsApp's servers): %v", err)
	}

	if err := partial.Close(); err != nil {
		return media, "", refuse(http.StatusInternalServerError, "Failed to save media file: %v", err)
	}
	if err := os.Rename(partial.Name(), localPath); err != nil {
		return media, "", refuse(http.StatusInternalServerError, "Failed to save media file: %v", err)
	}

	return media, localPath, nil
}

// Start a REST API server to expose the WhatsApp client functionality
func (b *Bridge) startRESTServer() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", b.guard(b.handleSend))
	mux.HandleFunc("/api/download", b.guard(b.handleDownload))

	// Listen on the loopback interface only, so nothing else on the network can reach the API
	serverAddr := fmt.Sprintf("127.0.0.1:%d", b.cfg.Port)
	listener, err := net.Listen("tcp", serverAddr)
	if err != nil {
		return fmt.Errorf("failed to start REST API server on %s: %v", serverAddr, err)
	}
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      200 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
	return nil
}
