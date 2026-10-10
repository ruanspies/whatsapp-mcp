package main

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"
)

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// StoredMessage is one row of the messages table
type StoredMessage struct {
	ID        string
	ChatJID   string
	Sender    string
	Content   string
	Timestamp time.Time
	IsFromMe  bool
	Media     MediaInfo
}

// MediaInfo is what is needed to show and later download an attachment
type MediaInfo struct {
	Type          string // image, video, audio, document or empty
	Filename      string // the sender's file name, for display only
	MimeType      string
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
}

// formatTimestamp renders a time the way every timestamp is stored: UTC, whole
// seconds, fixed width, so that comparing the stored strings compares the times.
func formatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05") + "+00:00"
}

// Initialize message store
func NewMessageStore(storeDir string) (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(storeDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}
	// Tighten a directory created by an earlier version
	if err := os.Chmod(storeDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to restrict store directory: %v", err)
	}

	// Open SQLite database for messages
	dbPath := filepath.Join(storeDir, "messages.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			direct_path TEXT,
			mime_type TEXT,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE INDEX IF NOT EXISTS messages_chat_time ON messages (chat_jid, timestamp);
		CREATE INDEX IF NOT EXISTS messages_time ON messages (timestamp);
		CREATE INDEX IF NOT EXISTS messages_sender ON messages (sender);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	store := &MessageStore{db: db}

	// Databases created by an earlier version lack the newer columns
	for _, column := range []string{"direct_path", "mime_type"} {
		if err := store.addMessageColumn(column); err != nil {
			db.Close()
			return nil, err
		}
	}

	if err := store.migrateTimestamps(); err != nil {
		db.Close()
		return nil, err
	}

	restrictFiles(storeDir)
	return store, nil
}

func (store *MessageStore) addMessageColumn(column string) error {
	var count int
	err := store.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = ?", column).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to inspect messages table: %v", err)
	}
	if count > 0 {
		return nil
	}
	if _, err := store.db.Exec("ALTER TABLE messages ADD COLUMN " + column + " TEXT"); err != nil {
		return fmt.Errorf("failed to add column %s: %v", column, err)
	}
	return nil
}

// migrateTimestamps rewrites timestamps written by earlier versions of the bridge,
// which stored local time with its offset, into the UTC format used now
func (store *MessageStore) migrateTimestamps() error {
	const current = "____-__-__ __:__:__+00:00"

	for _, target := range []struct{ table, column string }{{"messages", "timestamp"}, {"chats", "last_message_time"}} {
		rows, err := store.db.Query(
			"SELECT rowid, CAST("+target.column+" AS TEXT) FROM "+target.table+" WHERE "+target.column+" IS NOT NULL AND "+target.column+" NOT LIKE ?", current)
		if err != nil {
			return fmt.Errorf("failed to inspect %s timestamps: %v", target.table, err)
		}
		converted := map[int64]string{}
		for rows.Next() {
			var rowid int64
			var value string
			if err := rows.Scan(&rowid, &value); err != nil {
				rows.Close()
				return fmt.Errorf("failed to read %s timestamps: %v", target.table, err)
			}
			for _, layout := range sqlite3.SQLiteTimestampFormats {
				if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
					converted[rowid] = formatTimestamp(parsed)
					break
				}
			}
		}
		rows.Close()
		if len(converted) == 0 {
			continue
		}

		tx, err := store.db.Begin()
		if err != nil {
			return err
		}
		for rowid, value := range converted {
			if _, err := tx.Exec("UPDATE "+target.table+" SET "+target.column+" = ? WHERE rowid = ?", value, rowid); err != nil {
				tx.Rollback()
				return fmt.Errorf("failed to convert %s timestamps: %v", target.table, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// restrictFiles makes the files directly inside the store readable by the owner only
func restrictFiles(storeDir string) {
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			os.Chmod(filepath.Join(storeDir, entry.Name()), 0600)
		}
	}
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database, or move its last message time forward. The name
// is only used when the chat is new; SetChatName changes it afterwards.
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET last_message_time = excluded.last_message_time
		WHERE chats.last_message_time IS NULL OR excluded.last_message_time > chats.last_message_time`,
		jid, name, formatTimestamp(lastMessageTime),
	)
	return err
}

// SetChatName renames an existing chat. Chats that are not stored are left alone.
func (store *MessageStore) SetChatName(jid, name string) error {
	if name == "" {
		return nil
	}
	_, err := store.db.Exec("UPDATE chats SET name = ? WHERE jid = ? AND name IS NOT ?", name, jid, name)
	return err
}

// ChatName returns the stored name of a chat and whether the chat exists
func (store *MessageStore) ChatName(jid string) (string, bool) {
	var name sql.NullString
	err := store.db.QueryRow("SELECT name FROM chats WHERE jid = ?", jid).Scan(&name)
	if err != nil {
		return "", false
	}
	return name.String, true
}

// Store a message in the database. Message IDs are chosen by the sender, so a
// message that reuses an ID only replaces the stored one when it comes from the
// same sender; anyone else's is ignored.
func (store *MessageStore) StoreMessage(msg StoredMessage) error {
	// Only store if there's actual content or media
	if msg.Content == "" && msg.Media.Type == "" {
		return nil
	}

	// The length is whatever the sender declared, and SQLite integers are signed
	fileLength := int64(math.MaxInt64)
	if msg.Media.FileLength < math.MaxInt64 {
		fileLength = int64(msg.Media.FileLength)
	}

	_, err := store.db.Exec(
		`INSERT INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, direct_path, mime_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, chat_jid) DO UPDATE SET
			content = excluded.content, timestamp = excluded.timestamp, media_type = excluded.media_type, filename = excluded.filename,
			url = excluded.url, media_key = excluded.media_key, file_sha256 = excluded.file_sha256, file_enc_sha256 = excluded.file_enc_sha256,
			file_length = excluded.file_length, direct_path = excluded.direct_path, mime_type = excluded.mime_type
		WHERE messages.sender = excluded.sender AND messages.is_from_me = excluded.is_from_me`,
		msg.ID, msg.ChatJID, msg.Sender, msg.Content, formatTimestamp(msg.Timestamp), msg.IsFromMe,
		msg.Media.Type, msg.Media.Filename, msg.Media.URL, msg.Media.MediaKey, msg.Media.FileSHA256, msg.Media.FileEncSHA256,
		fileLength, msg.Media.DirectPath, msg.Media.MimeType,
	)
	return err
}

// UpdateMessageContent replaces the text of a message that its own sender edited
func (store *MessageStore) UpdateMessageContent(id, chatJID, sender string, isFromMe bool, content string) error {
	_, err := store.db.Exec("UPDATE messages SET content = ? WHERE id = ? AND chat_jid = ? AND sender = ? AND is_from_me = ?",
		content, id, chatJID, sender, isFromMe)
	return err
}

// DeleteMessage removes a message that its own sender deleted for everyone
func (store *MessageStore) DeleteMessage(id, chatJID, sender string, isFromMe bool) error {
	_, err := store.db.Exec("DELETE FROM messages WHERE id = ? AND chat_jid = ? AND sender = ? AND is_from_me = ?",
		id, chatJID, sender, isFromMe)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (MediaInfo, error) {
	var media MediaInfo
	var mediaType, filename, mimeType, url, directPath sql.NullString
	var fileLength sql.NullInt64

	err := store.db.QueryRow(
		"SELECT media_type, filename, mime_type, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &mimeType, &url, &directPath, &media.MediaKey, &media.FileSHA256, &media.FileEncSHA256, &fileLength)
	if err != nil {
		return media, err
	}

	media.Type = mediaType.String
	media.Filename = filename.String
	media.MimeType = mimeType.String
	media.URL = url.String
	media.DirectPath = directPath.String
	if fileLength.Int64 > 0 {
		media.FileLength = uint64(fileLength.Int64)
	}
	return media, nil
}

// queryStrings runs a query that returns a single text column
func (store *MessageStore) queryStrings(query string) ([]string, error) {
	rows, err := store.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// HiddenChats lists chats still stored under a hidden (@lid) address
func (store *MessageStore) HiddenChats() ([]string, error) {
	return store.queryStrings("SELECT jid FROM chats WHERE jid LIKE '%@lid'")
}

// HiddenSenders lists senders still stored under a hidden (@lid) address
func (store *MessageStore) HiddenSenders() ([]string, error) {
	return store.queryStrings("SELECT DISTINCT sender FROM messages WHERE sender LIKE '%@lid'")
}

// MergeChat moves every message of one chat into another and removes the first.
// It is used once the phone number behind a hidden address becomes known.
func (store *MessageStore) MergeChat(fromJID, toJID, fallbackName string) error {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Make sure the target exists, keeping the newer of the two last message times
	_, err = tx.Exec(
		`INSERT INTO chats (jid, name, last_message_time)
		SELECT ?, ?, last_message_time FROM chats WHERE jid = ?
		ON CONFLICT(jid) DO UPDATE SET last_message_time = excluded.last_message_time
		WHERE chats.last_message_time IS NULL OR excluded.last_message_time > chats.last_message_time`,
		toJID, fallbackName, fromJID,
	)
	if err != nil {
		return err
	}

	// Messages already present in the target win over their copies
	if _, err = tx.Exec("UPDATE OR IGNORE messages SET chat_jid = ? WHERE chat_jid = ?", toJID, fromJID); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM messages WHERE chat_jid = ?", fromJID); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM chats WHERE jid = ?", fromJID); err != nil {
		return err
	}

	return tx.Commit()
}

// RenameSender rewrites the sender of every message from one identity to another
func (store *MessageStore) RenameSender(from, to string) error {
	_, err := store.db.Exec("UPDATE messages SET sender = ? WHERE sender = ?", to, from)
	return err
}
