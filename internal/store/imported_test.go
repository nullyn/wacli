package store

import (
	"testing"
	"time"
)

func TestSyncedMessageReplacesImportedTwin(t *testing.T) {
	db := openTestDB(t)
	chat := "123@s.whatsapp.net"
	if err := db.UpsertChat(chat, "dm", "Sam", time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertImportedMessages(chat, "Sam", []ImportedMessage{
		{MsgID: "import-a", TS: 1000, Text: "Ok"},
		{MsgID: "import-b", TS: 1001, Text: "Ok"},
		{MsgID: "import-c", TS: 1000, Text: "Hi @\u2068Alex\u2069"},
	}); err != nil {
		t.Fatal(err)
	}
	upsert := func(id string, ts int64, text string) {
		t.Helper()
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: id, SenderJID: chat, Timestamp: time.Unix(ts, 0), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	upsert("REAL1", 999, "Ok")
	upsert("REAL1", 999, "Ok") // re-saving a known message must not consume the second twin
	upsert("REAL2", 1000, "Hi @919999999999")

	var ids []string
	rows, err := db.sql.Query(`SELECT msg_id FROM messages WHERE chat_jid = ? ORDER BY msg_id`, chat)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	want := []string{"REAL1", "REAL2", "import-b"}
	if len(ids) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got %v, want %v", ids, want)
		}
	}
}
