package main

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Read state tracks, per chat, the point up to which I have read it on any of my
// devices and whether I marked it as unread. It only ever moves forward, so the
// order in which WhatsApp delivers the signals does not matter. Nothing here
// tells WhatsApp that a chat was read.

// applyHistoryReadState sets the starting read point of a chat from history
// sync, which says how many of its latest messages are unread
func (b *Bridge) applyHistoryReadState(chat types.JID, unread uint32, markedUnread bool) {
	b.storeLock.Lock()
	defer b.storeLock.Unlock()

	chatJID := chat.String()
	if boundary, ok := b.store.UnreadBoundary(chatJID, int(unread)); ok {
		if err := b.store.AdvanceRead(chatJID, boundary); err != nil {
			b.logger.Warnf("Failed to store read state: %v", err)
		}
	}
	if markedUnread {
		if err := b.store.SetMarkedUnread(chatJID, true); err != nil {
			b.logger.Warnf("Failed to store read state: %v", err)
		}
	}
}

// handleReceipt records messages I read on another device. WhatsApp sends these
// as read receipts from my own account, or read-self when my read receipts are off.
func (b *Bridge) handleReceipt(evt *events.Receipt) {
	if !evt.IsFromMe || (evt.Type != types.ReceiptTypeRead && evt.Type != types.ReceiptTypeReadSelf) {
		return
	}
	if evt.Chat.Server == types.BroadcastServer || evt.Chat.Server == types.NewsletterServer {
		return
	}
	ctx := context.Background()

	b.storeLock.Lock()
	defer b.storeLock.Unlock()

	chatJID := b.resolveHidden(ctx, evt.Chat, evt.RecipientAlt).String()
	// The newest message read is the exact point; the time of reading is only a
	// fallback for messages that were never stored
	readUpTo, ok := b.store.LatestTimestamp(chatJID, evt.MessageIDs)
	if !ok {
		readUpTo = evt.Timestamp
	}
	if err := b.store.AdvanceRead(chatJID, readUpTo); err != nil {
		b.logger.Warnf("Failed to store read state: %v", err)
	}
	// Opening a chat clears a manual "mark as unread" even when nothing new was read
	if err := b.store.SetMarkedUnread(chatJID, false); err != nil {
		b.logger.Warnf("Failed to store read state: %v", err)
	}
}

// handleMarkChatAsRead records a chat being marked as read or unread from the
// chat list on another device
func (b *Bridge) handleMarkChatAsRead(evt *events.MarkChatAsRead) {
	ctx := context.Background()

	b.storeLock.Lock()
	defer b.storeLock.Unlock()

	chatJID := b.resolveHidden(ctx, evt.JID, types.EmptyJID).String()
	if !evt.Action.GetRead() {
		if err := b.store.SetMarkedUnread(chatJID, true); err != nil {
			b.logger.Warnf("Failed to store read state: %v", err)
		}
		return
	}

	readUpTo := evt.Timestamp
	if last := evt.Action.GetMessageRange().GetLastMessageTimestamp(); last > 0 {
		readUpTo = unixTime(last)
	}
	if err := b.store.AdvanceRead(chatJID, readUpTo); err != nil {
		b.logger.Warnf("Failed to store read state: %v", err)
	}
	if err := b.store.SetMarkedUnread(chatJID, false); err != nil {
		b.logger.Warnf("Failed to store read state: %v", err)
	}
}

// unixTime reads a WhatsApp timestamp, which is in seconds or, in some places,
// milliseconds
func unixTime(value int64) time.Time {
	if value > 1e12 {
		return time.UnixMilli(value)
	}
	return time.Unix(value, 0)
}

// catchUpAppState fetches chat setting changes made while the bridge was away.
// whatsmeow only fetches them when WhatsApp announces a change, which it does
// not always do for changes made while this device was offline.
func (b *Bridge) catchUpAppState(ctx context.Context) {
	for _, name := range appstate.AllPatchNames {
		if err := b.client.FetchAppState(ctx, name, false, false); err != nil {
			b.logger.Warnf("Failed to catch up on %s chat settings: %v", name, err)
		}
	}
}
