package main

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// pollCreation returns the poll a message starts, in any of the forms WhatsApp uses
func pollCreation(msg *waE2E.Message) *waE2E.PollCreationMessage {
	for _, poll := range []*waE2E.PollCreationMessage{
		msg.GetPollCreationMessage(),
		msg.GetPollCreationMessageV2(),
		msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV5(),
		msg.GetPollCreationMessageV6(),
		pollCreationV4(msg),
	} {
		if poll != nil {
			return poll
		}
	}
	return nil
}

func pollCreationV4(msg *waE2E.Message) *waE2E.PollCreationMessage {
	inner := msg.GetPollCreationMessageV4().GetMessage()
	if inner == nil || inner.GetPollCreationMessageV4() != nil {
		return nil
	}
	return pollCreation(inner)
}

func pollOptions(poll *waE2E.PollCreationMessage) []string {
	options := make([]string, 0, len(poll.GetOptions()))
	for _, option := range poll.GetOptions() {
		options = append(options, option.GetOptionName())
	}
	return options
}

// millisTime turns a WhatsApp millisecond timestamp into a time, or fallback when it is missing
func millisTime(ms int64, fallback time.Time) time.Time {
	if ms <= 0 {
		return fallback
	}
	return time.UnixMilli(ms)
}

// ingestVote records a vote that arrived as a message. Votes are encrypted with
// a secret of the poll, which whatsmeow saved when the poll was sent or received.
// Must be called with storeLock held.
func (b *Bridge) ingestVote(ctx context.Context, evt *events.Message, chatJID, sender string) {
	update := evt.Message.GetPollUpdateMessage()
	pollID := update.GetPollCreationMessageKey().GetID()
	if pollID == "" {
		return
	}
	vote, err := b.client.DecryptPollVote(ctx, evt)
	if err != nil {
		// Polls from before the bridge was linked, and not in its history, cannot be read
		b.logger.Debugf("Skipping vote %s on poll %s: %v", evt.Info.ID, pollID, err)
		return
	}
	votedAt := millisTime(update.GetSenderTimestampMS(), evt.Info.Timestamp)
	if err := b.store.StoreVote(pollID, chatJID, sender, evt.Info.IsFromMe, vote.GetSelectedOptions(), votedAt); err != nil {
		b.logger.Warnf("Failed to store vote: %v", err)
	}
}

// ingestHistoryVotes records the votes history carries with a poll. They come
// already decrypted, each with the key of the vote message, which names the voter.
// Must be called with storeLock held.
func (b *Bridge) ingestHistoryVotes(ctx context.Context, webMsg *waWeb.WebMessageInfo, chat types.JID, pollID string, fallback time.Time) {
	for _, update := range webMsg.GetPollUpdates() {
		voter := b.historyVoter(ctx, update.GetPollUpdateMessageKey(), chat)
		if voter == "" {
			continue
		}
		votedAt := millisTime(update.GetSenderTimestampMS(), millisTime(update.GetServerTimestampMS(), fallback))
		if err := b.store.StoreVote(pollID, chat.String(), voter, update.GetPollUpdateMessageKey().GetFromMe(), update.GetVote().GetSelectedOptions(), votedAt); err != nil {
			b.logger.Warnf("Failed to store vote: %v", err)
		}
	}
}

// historyVoter returns who cast a vote, stored the way message senders are
func (b *Bridge) historyVoter(ctx context.Context, key *waCommon.MessageKey, chat types.JID) string {
	if key.GetFromMe() {
		if b.client.Store.ID == nil {
			return ""
		}
		return b.client.Store.ID.User
	}
	address := key.GetParticipant()
	if address == "" && chat.Server != types.GroupServer {
		// In a direct chat the other person is the only one who can vote besides me
		address = key.GetRemoteJID()
		if address == "" {
			address = chat.String()
		}
	}
	jid, err := types.ParseJID(address)
	if err != nil || jid.User == "" {
		return ""
	}
	return senderString(b.resolveHidden(ctx, jid, types.EmptyJID))
}
