package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// at is sentAt moved by a number of minutes
func at(minutes int) time.Time {
	return sentAt.Add(time.Duration(minutes) * time.Minute)
}

func liveAt(id string, chat, sender types.JID, fromMe bool, when time.Time) *events.Message {
	evt := live(id, chat, sender, fromMe, text(id))
	evt.Info.Timestamp = when
	return evt
}

// readState returns the stored read point of a chat ("" when unknown) and
// whether it is marked unread
func (b *Bridge) readState(t *testing.T, jid types.JID) (string, bool) {
	t.Helper()
	var lastRead string
	var marked bool
	err := b.store.db.QueryRow(
		"SELECT COALESCE(CAST(last_read_at AS TEXT), ''), marked_unread FROM chats WHERE jid = ?", jid.String(),
	).Scan(&lastRead, &marked)
	if err != nil {
		t.Fatalf("read state of %s: %v", jid, err)
	}
	return lastRead, marked
}

func selfReceipt(chat types.JID, receiptType types.ReceiptType, when time.Time, ids ...string) *events.Receipt {
	return &events.Receipt{
		MessageSource: types.MessageSource{
			Chat: chat, Sender: types.NewADJID(me.User, 0, 31), IsFromMe: true, IsGroup: chat.Server == types.GroupServer,
		},
		MessageIDs: ids,
		Timestamp:  when,
		Type:       receiptType,
	}
}

func TestMyMessagesMarkTheChatRead(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()

	bridge.ingest(ctx, liveAt("A1", alice, alice, false, at(0)))
	if lastRead, _ := bridge.readState(t, alice); lastRead != "" {
		t.Errorf("read point before any reply = %q, want unknown", lastRead)
	}

	bridge.store.SetMarkedUnread(alice.String(), true)
	bridge.ingest(ctx, liveAt("M1", alice, me, true, at(1)))
	if lastRead, marked := bridge.readState(t, alice); lastRead != formatTimestamp(at(1)) || marked {
		t.Errorf("after my reply = %q marked %v, want %q unmarked", lastRead, marked, formatTimestamp(at(1)))
	}

	// An older message of mine arriving late, as history does, changes nothing
	bridge.store.SetMarkedUnread(alice.String(), true)
	bridge.ingest(ctx, liveAt("M0", alice, me, true, at(-5)))
	if lastRead, marked := bridge.readState(t, alice); lastRead != formatTimestamp(at(1)) || !marked {
		t.Errorf("after a late older reply = %q marked %v", lastRead, marked)
	}
}

func TestReadReceiptsFromMyOtherDevices(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()

	// A direct chat that WhatsApp addresses by hidden ID, stored under the phone number
	bridge.ingest(ctx, liveAt("A1", alice, alice, false, at(0)))
	bridge.ingest(ctx, liveAt("A2", alice, alice, false, at(1)))
	bridge.ingest(ctx, liveAt("A3", alice, alice, false, at(2)))

	// Alice reading my messages, and deliveries, are not my reads
	someoneElse := selfReceipt(aliceLID, types.ReceiptTypeRead, at(5), "A3")
	someoneElse.IsFromMe = false
	bridge.handleReceipt(someoneElse)
	bridge.handleReceipt(selfReceipt(aliceLID, types.ReceiptTypeDelivered, at(5), "A3"))
	bridge.handleReceipt(selfReceipt(aliceLID, types.ReceiptTypePlayed, at(5), "A3"))
	if lastRead, _ := bridge.readState(t, alice); lastRead != "" {
		t.Fatalf("read point after other receipts = %q, want unknown", lastRead)
	}

	// Reading up to A2 on the phone: the read point is A2, not the time of reading
	bridge.store.SetMarkedUnread(alice.String(), true)
	bridge.handleReceipt(selfReceipt(aliceLID, types.ReceiptTypeReadSelf, at(30), "A1", "A2"))
	if lastRead, marked := bridge.readState(t, alice); lastRead != formatTimestamp(at(1)) || marked {
		t.Errorf("after reading A1, A2 = %q marked %v, want %q unmarked", lastRead, marked, formatTimestamp(at(1)))
	}

	// A late receipt for an older message does not move it back
	bridge.handleReceipt(selfReceipt(aliceLID, types.ReceiptTypeRead, at(31), "A1"))
	if lastRead, _ := bridge.readState(t, alice); lastRead != formatTimestamp(at(1)) {
		t.Errorf("after a late receipt = %q", lastRead)
	}

	// A message that was never stored falls back to the time of reading
	bridge.handleReceipt(selfReceipt(alice, types.ReceiptTypeRead, at(40), "UNKNOWN"))
	if lastRead, _ := bridge.readState(t, alice); lastRead != formatTimestamp(at(40)) {
		t.Errorf("after reading an unstored message = %q, want %q", lastRead, formatTimestamp(at(40)))
	}

	// Groups work the same way
	bridge.ingest(ctx, liveAt("G1", familyJID, alice, false, at(3)))
	bridge.handleReceipt(selfReceipt(familyJID, types.ReceiptTypeRead, at(50), "G1"))
	if lastRead, _ := bridge.readState(t, familyJID); lastRead != formatTimestamp(at(3)) {
		t.Errorf("group read point = %q, want %q", lastRead, formatTimestamp(at(3)))
	}
}

func TestMarkChatAsReadAndUnread(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()
	bridge.ingest(ctx, liveAt("A1", alice, alice, false, at(0)))

	mark := func(read bool, lastMessage int64) {
		action := &waSyncAction.MarkChatAsReadAction{Read: proto.Bool(read)}
		if lastMessage != 0 {
			action.MessageRange = &waSyncAction.SyncActionMessageRange{LastMessageTimestamp: proto.Int64(lastMessage)}
		}
		bridge.handleMarkChatAsRead(&events.MarkChatAsRead{JID: aliceLID, Timestamp: at(60), Action: action})
	}

	mark(false, 0)
	if lastRead, marked := bridge.readState(t, alice); lastRead != "" || !marked {
		t.Errorf("after mark unread = %q marked %v", lastRead, marked)
	}

	mark(true, at(0).Unix())
	if lastRead, marked := bridge.readState(t, alice); lastRead != formatTimestamp(at(0)) || marked {
		t.Errorf("after mark read = %q marked %v, want %q unmarked", lastRead, marked, formatTimestamp(at(0)))
	}

	// Marking read without a message range uses the time of marking
	mark(false, 0)
	mark(true, 0)
	if lastRead, marked := bridge.readState(t, alice); lastRead != formatTimestamp(at(60)) || marked {
		t.Errorf("after mark read without range = %q marked %v", lastRead, marked)
	}
}

func TestHistorySetsTheStartingReadPoint(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()

	bridge.ingest(ctx, liveAt("M1", familyJID, me, true, at(0)))
	for i, id := range []string{"G1", "G2", "G3", "G4"} {
		bridge.ingest(ctx, liveAt(id, familyJID, alice, false, at(i+1)))
	}

	// Two unread: G3 and G4, so the read point is G2
	bridge.applyHistoryReadState(familyJID, 2, false)
	if lastRead, marked := bridge.readState(t, familyJID); lastRead != formatTimestamp(at(2)) || marked {
		t.Errorf("with 2 unread = %q marked %v, want %q", lastRead, marked, formatTimestamp(at(2)))
	}

	// None unread: everything is read
	bridge.applyHistoryReadState(familyJID, 0, true)
	if lastRead, marked := bridge.readState(t, familyJID); lastRead != formatTimestamp(at(4)) || !marked {
		t.Errorf("with none unread and marked = %q marked %v, want %q marked", lastRead, marked, formatTimestamp(at(4)))
	}

	// More unread than stored: just before the oldest stored message
	bridge.ingest(ctx, liveAt("A1", alice, alice, false, at(10)))
	bridge.applyHistoryReadState(alice, 5, false)
	if lastRead, _ := bridge.readState(t, alice); lastRead != formatTimestamp(at(10).Add(-time.Second)) {
		t.Errorf("with more unread than stored = %q", lastRead)
	}
}

func TestMergeChatKeepsReadState(t *testing.T) {
	bridge := newTestBridge(t)
	store := bridge.store
	hidden, phone, other := "111@lid", "27820000001@s.whatsapp.net", "222@lid"

	store.StoreChat(hidden, "111", at(0))
	store.StoreChat(phone, "Alice", at(0))
	store.AdvanceRead(hidden, at(5))
	store.AdvanceRead(phone, at(2))
	store.SetMarkedUnread(phone, true)
	if err := store.MergeChat(hidden, phone, "Alice"); err != nil {
		t.Fatal(err)
	}
	if lastRead, marked := bridge.readState(t, types.NewJID("27820000001", types.DefaultUserServer)); lastRead != formatTimestamp(at(5)) || !marked {
		t.Errorf("merged into existing = %q marked %v, want %q marked", lastRead, marked, formatTimestamp(at(5)))
	}

	// Merging into a chat that does not exist yet carries the read state over
	newPhone := types.NewJID("27820000002", types.DefaultUserServer)
	store.StoreChat(other, "222", at(0))
	store.AdvanceRead(other, at(7))
	store.SetMarkedUnread(other, true)
	if err := store.MergeChat(other, newPhone.String(), "Bob"); err != nil {
		t.Fatal(err)
	}
	if lastRead, marked := bridge.readState(t, newPhone); lastRead != formatTimestamp(at(7)) || !marked {
		t.Errorf("merged into new = %q marked %v, want %q marked", lastRead, marked, formatTimestamp(at(7)))
	}
}

func TestReadStateIsBackfilledFromMyReplies(t *testing.T) {
	bridge := newTestBridge(t)
	storeDir := bridge.cfg.StoreDir
	replied, silent := "27820000001@s.whatsapp.net", "27820000002@s.whatsapp.net"

	// A database from before read state: drop the columns the store just added
	db := bridge.store.db
	for _, column := range []string{"last_read_at", "marked_unread"} {
		if _, err := db.Exec("ALTER TABLE chats DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	for _, chat := range []string{replied, silent} {
		db.Exec("INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'x', ?)", chat, formatTimestamp(at(9)))
		db.Exec("INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('IN', ?, '1', 'hi', ?, 0)", chat, formatTimestamp(at(9)))
	}
	db.Exec("INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('ME1', ?, ?, 'a', ?, 1)", replied, me.User, formatTimestamp(at(3)))
	db.Exec("INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('ME2', ?, ?, 'b', ?, 1)", replied, me.User, formatTimestamp(at(4)))
	bridge.store.Close()

	reopened, err := NewMessageStore(storeDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	bridge.store = reopened

	if lastRead, marked := bridge.readState(t, types.NewJID("27820000001", types.DefaultUserServer)); lastRead != formatTimestamp(at(4)) || marked {
		t.Errorf("chat I replied in = %q marked %v, want %q", lastRead, marked, formatTimestamp(at(4)))
	}
	if lastRead, _ := bridge.readState(t, types.NewJID("27820000002", types.DefaultUserServer)); lastRead != "" {
		t.Errorf("chat I never replied in = %q, want unknown", lastRead)
	}
}
