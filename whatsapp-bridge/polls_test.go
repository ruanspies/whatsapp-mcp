package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func pollMessage(question string, selectable uint32, options ...string) *waE2E.PollCreationMessage {
	poll := &waE2E.PollCreationMessage{Name: proto.String(question), SelectableOptionsCount: proto.Uint32(selectable)}
	for _, option := range options {
		poll.Options = append(poll.Options, &waE2E.PollCreationMessage_Option{OptionName: proto.String(option)})
	}
	return poll
}

type storedPoll struct {
	question   string
	options    []string
	selectable uint32
}

func (b *Bridge) poll(t *testing.T, id string, chat types.JID) (storedPoll, bool) {
	t.Helper()
	var p storedPoll
	var options string
	err := b.store.db.QueryRow("SELECT question, options, selectable_count FROM polls WHERE message_id = ? AND chat_jid = ?", id, chat.String()).
		Scan(&p.question, &options, &p.selectable)
	if err != nil {
		return p, false
	}
	if err := json.Unmarshal([]byte(options), &p.options); err != nil {
		t.Fatalf("options of %s: %v", id, err)
	}
	return p, true
}

// votes returns each voter's selection as option texts, decoded from the stored hashes
func (b *Bridge) votes(t *testing.T, pollID string, chat types.JID, options ...string) map[string]string {
	t.Helper()
	names := map[string]string{}
	for i, hash := range whatsmeow.HashPollOptions(options) {
		names[hex.EncodeToString(hash)] = options[i]
	}
	rows, err := b.store.db.Query("SELECT voter, selected FROM poll_votes WHERE poll_id = ? AND chat_jid = ?", pollID, chat.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	votes := map[string]string{}
	for rows.Next() {
		var voter, selected string
		var hashes []string
		if err := rows.Scan(&voter, &selected); err != nil {
			t.Fatal(err)
		}
		json.Unmarshal([]byte(selected), &hashes)
		chosen := []string{}
		for _, hash := range hashes {
			chosen = append(chosen, names[hash])
		}
		votes[voter] = strings.Join(chosen, "+")
	}
	return votes
}

func TestPollsAreStored(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()

	forms := map[string]*waE2E.Message{
		"V1": {PollCreationMessage: pollMessage("Dinner?", 1, "Fri", "Sat")},
		"V3": {PollCreationMessageV3: pollMessage("Dinner?", 1, "Fri", "Sat")},
		"V4": {PollCreationMessageV4: &waE2E.FutureProofMessage{Message: &waE2E.Message{PollCreationMessage: pollMessage("Dinner?", 1, "Fri", "Sat")}}},
	}
	for id, msg := range forms {
		bridge.ingest(ctx, live(id, familyJID, types.NewADJID(alice.User, 0, 5), false, msg))
		if r, ok := bridge.row(t, id); !ok || r.content != "Dinner?" || r.sender != alice.User {
			t.Errorf("%s message = %+v, %v", id, r, ok)
		}
		if p, ok := bridge.poll(t, id, familyJID); !ok || p.question != "Dinner?" || strings.Join(p.options, ",") != "Fri,Sat" || p.selectable != 1 {
			t.Errorf("%s poll = %+v, %v", id, p, ok)
		}
	}

	// Someone else reusing the poll's ID cannot replace its options
	bridge.ingest(ctx, live("V1", familyJID, bobLID, false, &waE2E.Message{PollCreationMessage: pollMessage("Hijack", 0, "x", "y")}))
	if p, _ := bridge.poll(t, "V1", familyJID); p.question != "Dinner?" {
		t.Errorf("poll after a reused ID = %+v", p)
	}

	// Deleting the poll for everyone removes it and its votes
	bridge.store.StoreVote("V3", familyJID.String(), me.User, false, nil, sentAt)
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("V3")},
	}}
	bridge.ingest(ctx, live("DEL", familyJID, types.NewADJID(alice.User, 0, 5), false, revoke))
	if _, ok := bridge.poll(t, "V3", familyJID); ok {
		t.Error("deleted poll is still stored")
	}
	if votes := bridge.votes(t, "V3", familyJID); len(votes) != 0 {
		t.Errorf("votes of a deleted poll = %v", votes)
	}
}

func TestLiveVotes(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()
	options := []string{"Tue", "Thu"}

	bridge.ingest(ctx, live("P1", familyJID, types.NewADJID(alice.User, 0, 5), false, &waE2E.Message{
		PollCreationMessage: pollMessage("Meet when?", 0, options...),
	}))
	// whatsmeow saves the secret of every poll it receives; do what it would have done
	if err := bridge.client.Store.MsgSecrets.PutMessageSecret(ctx, familyJID, alice, "P1", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	pollInfo := &types.MessageInfo{MessageSource: types.MessageSource{Chat: familyJID, Sender: alice, IsGroup: true}, ID: "P1"}

	// My vote from my phone, encrypted exactly as WhatsApp does it
	vote := func(id string, when time.Time, chosen ...string) *events.Message {
		t.Helper()
		msg, err := bridge.client.BuildPollVote(ctx, pollInfo, chosen)
		if err != nil {
			t.Fatalf("BuildPollVote: %v", err)
		}
		msg.PollUpdateMessage.SenderTimestampMS = proto.Int64(when.UnixMilli())
		return live(id, familyJID, types.NewADJID(me.User, 0, 3), true, msg)
	}

	bridge.ingest(ctx, vote("VOTE1", at(1), "Tue"))
	if votes := bridge.votes(t, "P1", familyJID, options...); votes[me.User] != "Tue" {
		t.Fatalf("after voting = %v", votes)
	}
	if _, ok := bridge.row(t, "VOTE1"); ok {
		t.Error("a vote was stored as a message")
	}

	// Changing my vote replaces it; a late older vote does not
	bridge.ingest(ctx, vote("VOTE2", at(3), "Tue", "Thu"))
	bridge.ingest(ctx, vote("VOTE0", at(2), "Thu"))
	if votes := bridge.votes(t, "P1", familyJID, options...); votes[me.User] != "Tue+Thu" {
		t.Errorf("after changing = %v", votes)
	}

	// Withdrawing leaves an empty selection
	bridge.ingest(ctx, vote("VOTE3", at(4)))
	if votes := bridge.votes(t, "P1", familyJID, options...); votes[me.User] != "" || len(votes) != 1 {
		t.Errorf("after withdrawing = %v", votes)
	}

	// A vote on a poll whose secret is unknown is skipped
	unknown := vote("VOTE4", at(5), "Tue")
	unknown.Message.PollUpdateMessage.PollCreationMessageKey.ID = proto.String("NOPOLL")
	bridge.ingest(ctx, unknown)
	if votes := bridge.votes(t, "NOPOLL", familyJID); len(votes) != 0 {
		t.Errorf("votes on an unknown poll = %v", votes)
	}
}

func TestHistoryVotes(t *testing.T) {
	bridge := newIngestBridge(t)
	ctx := context.Background()
	options := []string{"Yes", "No"}
	hashes := whatsmeow.HashPollOptions(options)

	update := func(voterKey *waCommon.MessageKey, when time.Time, chosen ...[]byte) *waWeb.PollUpdate {
		return &waWeb.PollUpdate{
			PollUpdateMessageKey: voterKey,
			Vote:                 &waE2E.PollVoteMessage{SelectedOptions: chosen},
			SenderTimestampMS:    proto.Int64(when.UnixMilli()),
		}
	}
	history := func(chat types.JID, id, participant string, updates ...*waWeb.PollUpdate) {
		t.Helper()
		web := &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(participant == "")},
			Message:          &waE2E.Message{PollCreationMessageV3: pollMessage("Coming?", 1, options...)},
			MessageTimestamp: proto.Uint64(uint64(sentAt.Unix())),
			PollUpdates:      updates,
		}
		if participant != "" {
			web.Key.FromMe = proto.Bool(false)
			web.Participant = proto.String(participant)
		}
		evt, err := bridge.client.ParseWebMessage(chat, web)
		if err != nil {
			t.Fatalf("ParseWebMessage: %v", err)
		}
		bridge.ingest(ctx, evt)
	}

	// A group poll by Alice: Alice voted by hidden ID, Bob is unknown, I voted too
	history(familyJID, "HP1", alice.String(),
		update(&waCommon.MessageKey{FromMe: proto.Bool(false), Participant: proto.String(aliceLID.String())}, at(1), hashes[0]),
		update(&waCommon.MessageKey{FromMe: proto.Bool(false), Participant: proto.String(bobLID.String())}, at(2), hashes[1]),
		update(&waCommon.MessageKey{FromMe: proto.Bool(true)}, at(3), hashes[0]),
	)
	want := map[string]string{alice.User: "Yes", bobLID.User + "@lid": "No", me.User: "Yes"}
	if votes := bridge.votes(t, "HP1", familyJID, options...); !equalVotes(votes, want) {
		t.Errorf("group history votes = %v, want %v", votes, want)
	}

	// A direct poll of mine: the vote key names Alice only as the chat. Parsing
	// my own message in a hidden-ID chat needs my own hidden ID.
	bridge.client.Store.LID = types.NewJID("555000111222333", types.HiddenUserServer)
	history(aliceLID, "HP2", "",
		update(&waCommon.MessageKey{FromMe: proto.Bool(false), RemoteJID: proto.String(aliceLID.String())}, at(1), hashes[1]),
	)
	if votes := bridge.votes(t, "HP2", alice, options...); !equalVotes(votes, map[string]string{alice.User: "No"}) {
		t.Errorf("direct history votes = %v", votes)
	}
	if p, ok := bridge.poll(t, "HP2", alice); !ok || p.question != "Coming?" {
		t.Errorf("direct history poll = %+v, %v", p, ok)
	}

	// A newer live vote is not undone by history arriving late
	bridge.store.StoreVote("HP1", familyJID.String(), alice.User, false, [][]byte{hashes[1]}, at(10))
	history(familyJID, "HP1", alice.String(),
		update(&waCommon.MessageKey{FromMe: proto.Bool(false), Participant: proto.String(alice.String())}, at(1), hashes[0]),
	)
	if votes := bridge.votes(t, "HP1", familyJID, options...); votes[alice.User] != "No" {
		t.Errorf("after late history = %v", votes)
	}
}

func equalVotes(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for voter, choice := range want {
		if got[voter] != choice {
			return false
		}
	}
	return true
}

func TestPollsFollowIdentityCleanUp(t *testing.T) {
	bridge := newTestBridge(t)
	store := bridge.store
	hidden, phone := "111@lid", "27820000001@s.whatsapp.net"
	yes := [][]byte{{1}}

	store.StoreChat(hidden, "111", at(0))
	store.StoreMessage(StoredMessage{ID: "P1", ChatJID: hidden, Sender: "111@lid", Content: "Q", Timestamp: at(0)})
	store.StorePoll(StoredPoll{MessageID: "P1", ChatJID: hidden, Sender: "111@lid", Question: "Q", Options: []string{"a", "b"}})
	store.StoreVote("P1", hidden, "111@lid", false, yes, at(1))
	store.StoreVote("P1", hidden, me.User, true, yes, at(1))

	if senders, _ := store.HiddenSenders(); len(senders) != 1 || senders[0] != "111@lid" {
		t.Errorf("hidden senders = %v", senders)
	}
	if err := store.MergeChat(hidden, phone, "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := store.RenameSender("111@lid", "27820000001"); err != nil {
		t.Fatal(err)
	}
	var polls, votes, renamed int
	store.db.QueryRow("SELECT COUNT(*) FROM polls WHERE chat_jid = ?", phone).Scan(&polls)
	store.db.QueryRow("SELECT COUNT(*) FROM poll_votes WHERE chat_jid = ?", phone).Scan(&votes)
	store.db.QueryRow("SELECT COUNT(*) FROM poll_votes WHERE voter = '27820000001'").Scan(&renamed)
	if polls != 1 || votes != 2 || renamed != 1 {
		t.Errorf("after merge: %d polls, %d votes, %d renamed", polls, votes, renamed)
	}

	// Where both identities voted, the newer vote survives the rename
	store.StoreVote("P1", phone, "222@lid", false, [][]byte{{2}}, at(5))
	store.StoreVote("P1", phone, "27820000002", false, [][]byte{{3}}, at(4))
	store.RenameSender("222@lid", "27820000002")
	var selected string
	store.db.QueryRow("SELECT selected FROM poll_votes WHERE voter = '27820000002'").Scan(&selected)
	if selected != `["02"]` {
		t.Errorf("vote after clashing rename = %s, want the newer one", selected)
	}
}

func TestSendPollRules(t *testing.T) {
	known := "27820000001@s.whatsapp.net"
	twelve := `"1","2","3","4","5","6","7","8","9","10","11","12"`

	cases := []struct {
		name      string
		configure func(cfg *Config)
		body      string
		status    int
		contains  string
	}{
		{"read only", func(cfg *Config) { cfg.ReadOnly = true }, `{"recipient":"27820000001","question":"Q","options":["a","b"]}`, http.StatusForbidden, "read-only"},
		{"unknown chat", nil, `{"recipient":"27829999999","question":"Q","options":["a","b"]}`, http.StatusForbidden, "not an existing chat"},
		{"not on allowlist", func(cfg *Config) { cfg.SendAllowlist["120363000000000001@g.us"] = true }, `{"recipient":"27820000001","question":"Q","options":["a","b"]}`, http.StatusForbidden, "allowlist"},
		{"no recipient", nil, `{"question":"Q","options":["a","b"]}`, http.StatusBadRequest, "Recipient is required"},
		{"no question", nil, `{"recipient":"27820000001","question":"  ","options":["a","b"]}`, http.StatusBadRequest, "Question is required"},
		{"long question", nil, `{"recipient":"27820000001","question":"` + strings.Repeat("q", 256) + `","options":["a","b"]}`, http.StatusBadRequest, "longer than 255"},
		{"one option", nil, `{"recipient":"27820000001","question":"Q","options":["a"]}`, http.StatusBadRequest, "between 2 and 12"},
		{"thirteen options", nil, `{"recipient":"27820000001","question":"Q","options":[` + twelve + `,"13"]}`, http.StatusBadRequest, "between 2 and 12"},
		{"empty option", nil, `{"recipient":"27820000001","question":"Q","options":["a"," "]}`, http.StatusBadRequest, "Option 2 is empty"},
		{"long option", nil, `{"recipient":"27820000001","question":"Q","options":["a","` + strings.Repeat("o", 101) + `"]}`, http.StatusBadRequest, "longer than 100"},
		{"duplicate after trimming", nil, `{"recipient":"27820000001","question":"Q","options":["Fri","Fri "]}`, http.StatusBadRequest, "more than once"},
		// Allowed requests get as far as the WhatsApp client, which is not connected in tests
		{"allowed", nil, `{"recipient":"27820000001","question":"Q","options":[` + twelve + `],"multiple_answers":true}`, http.StatusServiceUnavailable, "Not connected"},
		{"on allowlist", func(cfg *Config) { cfg.SendAllowlist[known] = true }, `{"recipient":"` + known + `","question":"Q","options":["a","b"]}`, http.StatusServiceUnavailable, "Not connected"},
	}

	for _, c := range cases {
		bridge := newTestBridge(t)
		bridge.store.StoreChat(known, "Alice", time.Now())
		if c.configure != nil {
			c.configure(&bridge.cfg)
		}
		status, response := post(bridge.testServer(), "/api/send_poll", c.body, nil)
		if status != c.status || !strings.Contains(response.Message, c.contains) || response.Success {
			t.Errorf("%s: got %d %q, want %d containing %q", c.name, status, response.Message, c.status, c.contains)
		}
	}
}
