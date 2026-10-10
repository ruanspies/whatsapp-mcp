package main

import (
	"context"
	"fmt"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Bridge ties the WhatsApp client to the local message store and the REST API
type Bridge struct {
	cfg    Config
	client *whatsmeow.Client
	store  *MessageStore
	logger waLog.Logger
	token  string

	// Groups whose name is currently being fetched
	groupLookups sync.Map

	// Held while a message is stored and while hidden chats are merged, so a
	// chat cannot be merged away between choosing it and storing into it
	storeLock sync.Mutex
}

// Extract text content from a message, including the caption of media
func extractTextContent(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}

	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	} else if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	} else if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	} else if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}

	return ""
}

// Extract media info from a message
func extractMediaInfo(msg *waE2E.Message) MediaInfo {
	if msg == nil {
		return MediaInfo{}
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return MediaInfo{
			Type: "image", MimeType: img.GetMimetype(), URL: img.GetURL(), DirectPath: img.GetDirectPath(),
			MediaKey: img.GetMediaKey(), FileSHA256: img.GetFileSHA256(), FileEncSHA256: img.GetFileEncSHA256(), FileLength: img.GetFileLength(),
		}
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return MediaInfo{
			Type: "video", MimeType: vid.GetMimetype(), URL: vid.GetURL(), DirectPath: vid.GetDirectPath(),
			MediaKey: vid.GetMediaKey(), FileSHA256: vid.GetFileSHA256(), FileEncSHA256: vid.GetFileEncSHA256(), FileLength: vid.GetFileLength(),
		}
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return MediaInfo{
			Type: "audio", MimeType: aud.GetMimetype(), URL: aud.GetURL(), DirectPath: aud.GetDirectPath(),
			MediaKey: aud.GetMediaKey(), FileSHA256: aud.GetFileSHA256(), FileEncSHA256: aud.GetFileEncSHA256(), FileLength: aud.GetFileLength(),
		}
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = doc.GetTitle()
		}
		return MediaInfo{
			Type: "document", Filename: filename, MimeType: doc.GetMimetype(), URL: doc.GetURL(), DirectPath: doc.GetDirectPath(),
			MediaKey: doc.GetMediaKey(), FileSHA256: doc.GetFileSHA256(), FileEncSHA256: doc.GetFileEncSHA256(), FileLength: doc.GetFileLength(),
		}
	}

	return MediaInfo{}
}

// resolveHidden returns the phone number address behind a hidden (@lid) address
// when it is known, and the address unchanged otherwise. WhatsApp addresses many
// chats and group members by hidden ID; storing the phone number keeps one chat
// and one sender per person.
func (b *Bridge) resolveHidden(ctx context.Context, jid, alt types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD()
	}
	if b.client.Store.LIDs == nil {
		return jid
	}
	pn, err := b.client.Store.LIDs.GetPNForLID(ctx, jid)
	if err == nil && pn.User != "" {
		return pn.ToNonAD()
	}
	return jid
}

// chatJID returns the address a message's chat is stored under
func (b *Bridge) chatJID(ctx context.Context, info types.MessageInfo) types.JID {
	// In a direct chat the other party is the recipient of my messages and the sender of theirs
	alt := info.SenderAlt
	if info.IsFromMe {
		alt = info.RecipientAlt
	}
	return b.resolveHidden(ctx, info.Chat, alt)
}

// senderID returns how the sender of a message is stored: the bare phone number,
// or the full hidden address when the phone number is not known yet
func (b *Bridge) senderID(ctx context.Context, info types.MessageInfo) string {
	if info.IsFromMe && b.client.Store.ID != nil {
		return b.client.Store.ID.User
	}
	return senderString(b.resolveHidden(ctx, info.Sender, info.SenderAlt))
}

func senderString(jid types.JID) string {
	if jid.Server == types.HiddenUserServer {
		return jid.User + "@" + types.HiddenUserServer
	}
	return jid.User
}

// ingest stores one message, live or from history, and returns the chat it was
// stored under. The chat is empty when the message was not stored.
func (b *Bridge) ingest(ctx context.Context, evt *events.Message) types.JID {
	info := evt.Info
	msg := evt.Message
	if msg == nil || info.Chat.User == "" {
		return types.EmptyJID
	}

	// Status updates and channels are not conversations
	if info.Chat.Server == types.BroadcastServer || info.Chat.Server == types.NewsletterServer {
		return types.EmptyJID
	}

	if wrapped := msg.GetDocumentWithCaptionMessage().GetMessage(); wrapped != nil {
		msg = wrapped
	}

	b.storeLock.Lock()
	defer b.storeLock.Unlock()

	chat := b.chatJID(ctx, info)
	chatJID := chat.String()
	sender := b.senderID(ctx, info)

	// Edits and deletions change a message that is already stored, and only its own sender may make them
	if protocol := msg.GetProtocolMessage(); protocol != nil {
		targetID := protocol.GetKey().GetID()
		switch protocol.GetType() {
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if content := extractTextContent(protocol.GetEditedMessage()); content != "" && targetID != "" {
				if err := b.store.UpdateMessageContent(targetID, chatJID, sender, info.IsFromMe, content); err != nil {
					b.logger.Warnf("Failed to store edit: %v", err)
				}
			}
		case waE2E.ProtocolMessage_REVOKE:
			if targetID != "" {
				if err := b.store.DeleteMessage(targetID, chatJID, sender, info.IsFromMe); err != nil {
					b.logger.Warnf("Failed to store deletion: %v", err)
				}
			}
		}
		return types.EmptyJID
	}

	content := extractTextContent(msg)
	media := extractMediaInfo(msg)

	// Skip if there's no content and no media
	if content == "" && media.Type == "" {
		return types.EmptyJID
	}

	// The chat has to exist before its messages. Until a better name is known it is named after its address.
	if err := b.store.StoreChat(chatJID, chat.User, info.Timestamp); err != nil {
		b.logger.Warnf("Failed to store chat: %v", err)
		return types.EmptyJID
	}

	err := b.store.StoreMessage(StoredMessage{
		ID:        info.ID,
		ChatJID:   chatJID,
		Sender:    sender,
		Content:   content,
		Timestamp: info.Timestamp,
		IsFromMe:  info.IsFromMe,
		Media:     media,
	})
	if err != nil {
		b.logger.Warnf("Failed to store message: %v", err)
		return types.EmptyJID
	}

	return chat
}

// Handle regular incoming messages
func (b *Bridge) handleMessage(evt *events.Message) {
	ctx := context.Background()
	if chat := b.ingest(ctx, evt); chat.User != "" {
		b.refreshChatName(ctx, chat)
	}
}

// Handle history sync events
func (b *Bridge) handleHistorySync(historySync *events.HistorySync) {
	ctx := context.Background()
	conversations := historySync.Data.GetConversations()
	fmt.Printf("Received history sync event with %d conversations\n", len(conversations))

	syncedCount := 0
	for _, conversation := range conversations {
		conversationJID, err := types.ParseJID(conversation.GetID())
		if err != nil || conversationJID.User == "" {
			b.logger.Warnf("Skipping conversation with unreadable ID: %v", err)
			continue
		}

		var chat types.JID
		for _, historyMsg := range conversation.GetMessages() {
			if historyMsg.GetMessage() == nil {
				continue
			}
			evt, err := b.client.ParseWebMessage(conversationJID, historyMsg.GetMessage())
			if err != nil {
				continue
			}
			if stored := b.ingest(ctx, evt); stored.User != "" {
				chat = stored
				syncedCount++
			}
		}

		if chat.User == "" {
			continue
		}

		// History carries group names directly; contacts come from the contact store
		if chat.Server == types.GroupServer && conversation.GetName() != "" {
			if err := b.store.SetChatName(chat.String(), conversation.GetName()); err != nil {
				b.logger.Warnf("Failed to name chat: %v", err)
			}
		} else {
			b.refreshChatName(ctx, chat)
		}
	}

	b.mergeHiddenIdentities(ctx)
	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// contactName picks the best known name for a person, or "" when there is none
func (b *Bridge) contactName(ctx context.Context, jid types.JID) string {
	if b.client.Store.Contacts == nil {
		return ""
	}
	contact, err := b.client.Store.Contacts.GetContact(ctx, jid)
	if err != nil {
		return ""
	}
	for _, name := range []string{contact.FullName, contact.FirstName, contact.PushName, contact.BusinessName} {
		if name != "" {
			return name
		}
	}
	return ""
}

// refreshChatName brings the stored name of a chat up to date
func (b *Bridge) refreshChatName(ctx context.Context, chat types.JID) {
	chatJID := chat.String()

	if chat.Server == types.GroupServer {
		// Group names need a round trip to WhatsApp, so only ask while the name is unknown
		name, exists := b.store.ChatName(chatJID)
		if !exists || (name != "" && name != chat.User) {
			return
		}
		if _, busy := b.groupLookups.LoadOrStore(chatJID, true); busy {
			return
		}
		go func() {
			defer b.groupLookups.Delete(chatJID)
			groupInfo, err := b.client.GetGroupInfo(context.Background(), chat)
			if err != nil || groupInfo == nil {
				return
			}
			if err := b.store.SetChatName(chatJID, groupInfo.Name); err != nil {
				b.logger.Warnf("Failed to name chat: %v", err)
			}
		}()
		return
	}

	if err := b.store.SetChatName(chatJID, b.contactName(ctx, chat)); err != nil {
		b.logger.Warnf("Failed to name chat: %v", err)
	}
}

// handleContactChange renames a direct chat when the name of its contact changes
func (b *Bridge) handleContactChange(jids ...types.JID) {
	ctx := context.Background()
	for _, jid := range jids {
		if jid.User == "" {
			continue
		}
		chat := b.resolveHidden(ctx, jid, types.EmptyJID)
		name := b.contactName(ctx, chat)
		if name == "" {
			// Names learned under the hidden address still belong to this person
			name = b.contactName(ctx, jid.ToNonAD())
		}
		if err := b.store.SetChatName(chat.String(), name); err != nil {
			b.logger.Warnf("Failed to name chat: %v", err)
		}
	}
}

// loadGroupNames names every group the account is in with one request
func (b *Bridge) loadGroupNames(ctx context.Context) {
	groups, err := b.client.GetJoinedGroups(ctx)
	if err != nil {
		b.logger.Warnf("Failed to load group names: %v", err)
		return
	}
	for _, group := range groups {
		if err := b.store.SetChatName(group.JID.String(), group.Name); err != nil {
			b.logger.Warnf("Failed to name chat: %v", err)
		}
	}
}

// mergeHiddenIdentities moves chats and senders stored under a hidden address to
// the phone number, for every hidden address whose phone number is now known
func (b *Bridge) mergeHiddenIdentities(ctx context.Context) {
	b.storeLock.Lock()
	defer b.storeLock.Unlock()

	chats, err := b.store.HiddenChats()
	if err != nil {
		b.logger.Warnf("Failed to list hidden chats: %v", err)
	}
	for _, chatJID := range chats {
		hidden, err := types.ParseJID(chatJID)
		if err != nil {
			continue
		}
		resolved := b.resolveHidden(ctx, hidden, types.EmptyJID)
		if resolved.Server == types.HiddenUserServer {
			continue
		}
		if err := b.store.MergeChat(chatJID, resolved.String(), resolved.User); err != nil {
			b.logger.Warnf("Failed to merge chat: %v", err)
			continue
		}
		b.refreshChatName(ctx, resolved)
	}

	senders, err := b.store.HiddenSenders()
	if err != nil {
		b.logger.Warnf("Failed to list hidden senders: %v", err)
	}
	for _, sender := range senders {
		hidden, err := types.ParseJID(sender)
		if err != nil {
			continue
		}
		resolved := b.resolveHidden(ctx, hidden, types.EmptyJID)
		if resolved.Server == types.HiddenUserServer {
			continue
		}
		if err := b.store.RenameSender(sender, resolved.User); err != nil {
			b.logger.Warnf("Failed to rename sender: %v", err)
		}
	}
}
