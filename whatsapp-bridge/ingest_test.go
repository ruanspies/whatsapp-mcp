package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var (
	me        = types.NewJID("27820000000", types.DefaultUserServer)
	alice     = types.NewJID("27820000001", types.DefaultUserServer)
	aliceLID  = types.NewJID("111222333444555", types.HiddenUserServer)
	bobLID    = types.NewJID("999888777666555", types.HiddenUserServer)
	familyJID = types.NewJID("120363000000000001", types.GroupServer)
	sentAt    = time.Date(2026, 10, 10, 15, 4, 5, 0, time.UTC)
)

// newIngestBridge builds a bridge around a real, offline WhatsApp client
func newIngestBridge(t *testing.T) *Bridge {
	t.Helper()
	bridge := newTestBridge(t)
	ctx := context.Background()

	sessionDB := filepath.Join(bridge.cfg.StoreDir, "whatsapp.db")
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+sessionDB+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New: %v", err)
	}
	t.Cleanup(func() { container.Close() })

	device := container.NewDevice()
	own := types.NewADJID(me.User, 0, 7)
	device.ID = &own
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details: []byte{0}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64),
	}
	// Saving the device is what attaches the contact and identity stores, as pairing does
	if err := device.Save(ctx); err != nil {
		t.Fatalf("device.Save: %v", err)
	}
	bridge.client = whatsmeow.NewClient(device, waLog.Noop)

	// Alice's hidden address is known; Bob's is not
	if err := device.LIDs.PutLIDMapping(ctx, aliceLID, alice); err != nil {
		t.Fatalf("PutLIDMapping: %v", err)
	}
	return bridge
}

func text(body string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(body)}
}

func live(id string, chat, sender types.JID, fromMe bool, msg *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: fromMe, IsGroup: chat.Server == types.GroupServer},
			ID:            id,
			Timestamp:     sentAt,
		},
		Message: msg,
	}
}

type storedRow struct {
	chat, sender, content, mediaType, filename, timestamp string
	fromMe                                                bool
}

func (b *Bridge) row(t *testing.T, id string) (storedRow, bool) {
	t.Helper()
	var r storedRow
	err := b.store.db.QueryRow(
		"SELECT chat_jid, sender, content, COALESCE(media_type, ''), COALESCE(filename, ''), CAST(timestamp AS TEXT), is_from_me FROM messages WHERE id = ?", id,
	).Scan(&r.chat, &r.sender, &r.content, &r.mediaType, &r.filename, &r.timestamp, &r.fromMe)
	return r, err == nil
}

func TestIngestLiveMessages(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()

	// Direct chat addressed by phone number
	bridge.ingest(ctx, live("DM1", alice, alice, false, text("hello")))
	if r, ok := bridge.row(t, "DM1"); !ok || r.chat != alice.String() || r.sender != alice.User || r.content != "hello" || r.fromMe || r.timestamp != "2026-10-10 15:04:05+00:00" {
		t.Errorf("DM1 = %+v, %v", r, ok)
	}

	// My own message is the first one seen in a chat: the chat must not be named after me
	carol := types.NewJID("27820000003", types.DefaultUserServer)
	bridge.ingest(ctx, live("DM2", carol, types.NewADJID(me.User, 0, 3), true, text("hi Carol")))
	if r, _ := bridge.row(t, "DM2"); r.sender != me.User || !r.fromMe {
		t.Errorf("DM2 = %+v", r)
	}
	if name, _ := bridge.store.ChatName(carol.String()); name != carol.User {
		t.Errorf("chat with Carol is named %q, want her number until a name is known", name)
	}

	// Direct chat addressed by hidden ID, phone number known from the mapping
	bridge.ingest(ctx, live("DM3", aliceLID, aliceLID, false, text("from hidden address")))
	if r, _ := bridge.row(t, "DM3"); r.chat != alice.String() || r.sender != alice.User {
		t.Errorf("DM3 = %+v, want it in the phone number chat", r)
	}

	// Direct chat addressed by hidden ID, phone number supplied with the message
	dave := types.NewJID("27820000004", types.DefaultUserServer)
	daveLID := types.NewJID("444333222111000", types.HiddenUserServer)
	fromDave := live("DM4", daveLID, daveLID, false, text("hi"))
	fromDave.Info.SenderAlt = dave
	bridge.ingest(ctx, fromDave)
	toDave := live("DM5", daveLID, types.NewJID("555000111222333", types.HiddenUserServer), true, text("hi Dave"))
	toDave.Info.RecipientAlt = dave
	bridge.ingest(ctx, toDave)
	for _, id := range []string{"DM4", "DM5"} {
		if r, _ := bridge.row(t, id); r.chat != dave.String() {
			t.Errorf("%s stored in chat %q, want %q", id, r.chat, dave.String())
		}
	}

	// Group messages: device suffix dropped, hidden senders resolved when possible
	bridge.ingest(ctx, live("G1", familyJID, types.NewADJID(alice.User, 0, 5), false, text("one")))
	bridge.ingest(ctx, live("G2", familyJID, aliceLID, false, text("two")))
	bridge.ingest(ctx, live("G3", familyJID, bobLID, false, text("three")))
	for id, want := range map[string]string{"G1": alice.User, "G2": alice.User, "G3": bobLID.User + "@lid"} {
		if r, _ := bridge.row(t, id); r.sender != want || r.chat != familyJID.String() {
			t.Errorf("%s = %+v, want sender %q", id, r, want)
		}
	}

	// Captions are the text of media messages
	bridge.ingest(ctx, live("M1", alice, alice, false, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: proto.String("the invoice"), Mimetype: proto.String("image/jpeg"), DirectPath: proto.String("/v/x.enc"),
	}}))
	bridge.ingest(ctx, live("M2", alice, alice, false, &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
		Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Caption: proto.String("signed copy"), FileName: proto.String("../../evil.pdf"), Mimetype: proto.String("application/pdf"),
		}},
	}}))
	bridge.ingest(ctx, live("M3", alice, alice, false, &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg; codecs=opus")}}))
	if r, _ := bridge.row(t, "M1"); r.content != "the invoice" || r.mediaType != "image" || r.filename != "" {
		t.Errorf("M1 = %+v", r)
	}
	if r, _ := bridge.row(t, "M2"); r.content != "signed copy" || r.mediaType != "document" || r.filename != "../../evil.pdf" {
		t.Errorf("M2 = %+v", r)
	}
	if r, ok := bridge.row(t, "M3"); !ok || r.mediaType != "audio" || r.content != "" {
		t.Errorf("M3 = %+v, %v", r, ok)
	}

	// Not stored: status updates, channels, and messages with nothing to show
	bridge.ingest(ctx, live("S1", types.StatusBroadcastJID, alice, false, text("my story")))
	bridge.ingest(ctx, live("S2", types.NewJID("123", types.NewsletterServer), alice, false, text("channel post")))
	bridge.ingest(ctx, live("S3", alice, alice, false, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}))
	for _, id := range []string{"S1", "S2", "S3"} {
		if r, ok := bridge.row(t, id); ok {
			t.Errorf("%s was stored: %+v", id, r)
		}
	}

	// Edits and deletions change the stored message
	edit := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("DM1")}, EditedMessage: text("hello there"),
	}}
	bridge.ingest(ctx, live("E1", alice, alice, false, edit))
	if r, _ := bridge.row(t, "DM1"); r.content != "hello there" {
		t.Errorf("after edit DM1 = %q", r.content)
	}
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("DM1")},
	}}
	bridge.ingest(ctx, live("E2", alice, alice, false, revoke))
	if _, ok := bridge.row(t, "DM1"); ok {
		t.Errorf("DM1 still stored after it was deleted for everyone")
	}
}

func TestHistoryMatchesLive(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()

	webMessage := func(id string, fromMe bool, participant string, msg *waE2E.Message) *waWeb.WebMessageInfo {
		info := &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe)},
			Message:          msg,
			MessageTimestamp: proto.Uint64(uint64(sentAt.Unix())),
		}
		if participant != "" {
			info.Participant = proto.String(participant)
		}
		return info
	}
	ingestHistory := func(chat types.JID, web *waWeb.WebMessageInfo) {
		t.Helper()
		evt, err := bridge.client.ParseWebMessage(chat, web)
		if err != nil {
			t.Fatalf("ParseWebMessage: %v", err)
		}
		bridge.ingest(ctx, evt)
	}

	ingestHistory(familyJID, webMessage("H1", false, alice.String(), text("old one")))
	ingestHistory(familyJID, webMessage("H2", false, aliceLID.String(), text("old two")))
	ingestHistory(familyJID, webMessage("H3", true, "", text("old mine")))
	ingestHistory(aliceLID, webMessage("H4", false, "", text("old direct")))
	ingestHistory(bobLID, webMessage("H5", false, "", text("from Bob")))
	bridge.ingest(ctx, live("L1", familyJID, types.NewADJID(alice.User, 0, 5), false, text("new one")))

	want := map[string]storedRow{
		"H1": {chat: familyJID.String(), sender: alice.User},
		"H2": {chat: familyJID.String(), sender: alice.User},
		"H3": {chat: familyJID.String(), sender: me.User, fromMe: true},
		"H4": {chat: alice.String(), sender: alice.User},
		"H5": {chat: bobLID.String(), sender: bobLID.User + "@lid"},
		"L1": {chat: familyJID.String(), sender: alice.User},
	}
	for id, w := range want {
		r, ok := bridge.row(t, id)
		if !ok || r.chat != w.chat || r.sender != w.sender || r.fromMe != w.fromMe || r.timestamp != "2026-10-10 15:04:05+00:00" {
			t.Errorf("%s = %+v (stored %v), want chat %q sender %q", id, r, ok, w.chat, w.sender)
		}
	}

	// Once Bob's phone number is learned, his chat and messages move to it
	bob := types.NewJID("27820000002", types.DefaultUserServer)
	if err := bridge.client.Store.LIDs.PutLIDMapping(ctx, bobLID, bob); err != nil {
		t.Fatal(err)
	}
	bridge.ingest(ctx, live("G3", familyJID, bobLID, false, text("in the group")))
	bridge.store.RenameSender(bob.User, bobLID.User+"@lid") // as if stored before the number was known
	bridge.mergeHiddenIdentities(ctx)

	if r, _ := bridge.row(t, "H5"); r.chat != bob.String() || r.sender != bob.User {
		t.Errorf("after merge H5 = %+v", r)
	}
	if r, _ := bridge.row(t, "G3"); r.sender != bob.User {
		t.Errorf("after merge G3 sender = %q", r.sender)
	}
	if chats, _ := bridge.store.HiddenChats(); len(chats) != 0 {
		t.Errorf("hidden chats remain: %v", chats)
	}
}

func TestContactNamesReachChats(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()
	contacts := bridge.client.Store.Contacts

	bridge.handleMessage(live("N1", alice, alice, false, text("hi")))
	if name, _ := bridge.store.ChatName(alice.String()); name != alice.User {
		t.Errorf("unnamed contact: chat is %q, want the number", name)
	}

	contacts.PutPushName(ctx, alice, "ali")
	bridge.handleMessage(live("N2", alice, alice, false, text("hi again")))
	if name, _ := bridge.store.ChatName(alice.String()); name != "ali" {
		t.Errorf("after push name: %q", name)
	}

	contacts.PutContactName(ctx, alice, "Alice", "Alice Adams")
	bridge.handleContactChange(aliceLID) // the event may name her by hidden address
	if name, _ := bridge.store.ChatName(alice.String()); name != "Alice Adams" {
		t.Errorf("after address book name: %q", name)
	}

	// A later push name does not replace the address book name
	contacts.PutPushName(ctx, alice, "ali2")
	bridge.handleContactChange(alice)
	if name, _ := bridge.store.ChatName(alice.String()); name != "Alice Adams" {
		t.Errorf("push name replaced the address book name: %q", name)
	}
}

func TestOnlyTheSenderCanChangeAMessage(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()
	bob := types.NewJID("27820000002", types.DefaultUserServer)
	myDevice := types.NewADJID(me.User, 0, 3)

	bridge.ingest(ctx, live("ALICE1", familyJID, alice, false, text("from Alice")))
	bridge.ingest(ctx, live("MINE1", familyJID, myDevice, true, text("from me")))

	protocol := func(kind waE2E.ProtocolMessage_Type, target string, edited *waE2E.Message) *waE2E.Message {
		return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: kind.Enum(), Key: &waCommon.MessageKey{ID: proto.String(target)}, EditedMessage: edited}}
	}

	// Bob tries to rewrite, delete and overwrite messages that are not his
	for _, target := range []string{"ALICE1", "MINE1"} {
		bridge.ingest(ctx, live("X1"+target, familyJID, bob, false, protocol(waE2E.ProtocolMessage_MESSAGE_EDIT, target, text("FORGED"))))
		bridge.ingest(ctx, live("X2"+target, familyJID, bob, false, protocol(waE2E.ProtocolMessage_REVOKE, target, nil)))
		bridge.ingest(ctx, live(target, familyJID, bob, false, text("REPLACED")))
	}
	if r, ok := bridge.row(t, "ALICE1"); !ok || r.content != "from Alice" || r.sender != alice.User {
		t.Errorf("ALICE1 = %+v (stored %v)", r, ok)
	}
	if r, ok := bridge.row(t, "MINE1"); !ok || r.content != "from me" || !r.fromMe || r.sender != me.User {
		t.Errorf("MINE1 = %+v (stored %v)", r, ok)
	}

	// The real senders still can
	bridge.ingest(ctx, live("E1", familyJID, alice, false, protocol(waE2E.ProtocolMessage_MESSAGE_EDIT, "ALICE1", text("Alice, edited"))))
	if r, _ := bridge.row(t, "ALICE1"); r.content != "Alice, edited" {
		t.Errorf("Alice's own edit was not applied: %q", r.content)
	}
	bridge.ingest(ctx, live("E2", familyJID, myDevice, true, protocol(waE2E.ProtocolMessage_REVOKE, "MINE1", nil)))
	if _, ok := bridge.row(t, "MINE1"); ok {
		t.Errorf("my own deletion was not applied")
	}
	// A message delivered twice by its sender is stored once, with the latest content
	bridge.ingest(ctx, live("ALICE1", familyJID, alice, false, text("redelivered")))
	if r, _ := bridge.row(t, "ALICE1"); r.content != "redelivered" {
		t.Errorf("redelivery by the sender = %q", r.content)
	}
}

func TestIngestDuringMergeLosesNothing(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()
	const perWorker, workers = 150, 6

	done := make(chan struct{})
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < perWorker; i++ {
				// A hidden chat whose number becomes known half way through
				hidden := types.NewJID(fmt.Sprintf("7000000%02d", w), types.HiddenUserServer)
				if i == perWorker/2 {
					bridge.client.Store.LIDs.PutLIDMapping(ctx, hidden, types.NewJID(fmt.Sprintf("2783000%02d", w), types.DefaultUserServer))
				}
				bridge.ingest(ctx, live(fmt.Sprintf("W%d-%d", w, i), hidden, hidden, false, text("x")))
			}
		}(w)
	}
	stop := make(chan struct{})
	merged := make(chan struct{})
	go func() {
		defer close(merged)
		for {
			select {
			case <-stop:
				return
			default:
				bridge.mergeHiddenIdentities(ctx)
			}
		}
	}()
	for w := 0; w < workers; w++ {
		<-done
	}
	close(stop)
	<-merged
	bridge.mergeHiddenIdentities(ctx)

	var count int
	bridge.store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if count != perWorker*workers {
		t.Errorf("stored %d of %d messages", count, perWorker*workers)
	}
	if chats, _ := bridge.store.HiddenChats(); len(chats) != 0 {
		t.Errorf("hidden chats remain: %v", chats)
	}
}
