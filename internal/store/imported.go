package store

import (
	"database/sql"
	"regexp"
	"strings"
)

// ImportedMsgIDPrefix marks messages created from a WhatsApp chat export.
// These IDs are synthetic and must never be sent to WhatsApp.
const ImportedMsgIDPrefix = "import-"

func IsImportedMsgID(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), ImportedMsgIDPrefix)
}

var (
	importMention   = regexp.MustCompile(`@\x{2068}[^\x{2069}]*\x{2069}|@\d{6,}`)
	importInvisible = strings.NewReplacer("\u200e", "", "\u2060", "", "\u2068", "", "\u2069", "", "\u202f", " ", "\r", "")
	importSpaces    = regexp.MustCompile(`\s+`)
)

// ImportMatchKey normalizes message text so an export line and the same
// message synced from WhatsApp compare equal: mentions, invisible marks,
// the edited marker and whitespace differ between the two.
func ImportMatchKey(s string) string {
	s = importMention.ReplaceAllString(s, "@")
	s = importInvisible.Replace(strings.TrimSuffix(strings.TrimSpace(s), "<This message was edited>"))
	return strings.TrimSpace(importSpaces.ReplaceAllString(s, " "))
}

// importTwinWindow matches waexport.Window: export and synced timestamps
// differ by at most a second in practice.
const importTwinWindow = 2

// dropImportedTwins deletes imported copies of a message that has now been
// stored with its real WhatsApp ID, so a later sync doesn't duplicate gap-fill
// imports.
func (d *DB) dropImportedTwins(chatJID string, fromMe bool, ts int64, text string) error {
	key := ImportMatchKey(text)
	if key == "" {
		return nil
	}
	rows, err := d.sql.Query(`SELECT msg_id, COALESCE(text,'') FROM messages
		WHERE chat_jid = ? AND ts BETWEEN ? AND ? AND from_me = ? AND msg_id LIKE 'import-%'`,
		chatJID, ts-importTwinWindow, ts+importTwinWindow, boolToInt64(fromMe))
	if err != nil {
		return err
	}
	var twins []string
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			rows.Close()
			return err
		}
		if ImportMatchKey(t) == key {
			twins = append(twins, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// One real message replaces at most one imported copy.
	if len(twins) == 0 {
		return nil
	}
	_, err = d.sql.Exec(`DELETE FROM messages WHERE chat_jid = ? AND msg_id = ?`, chatJID, twins[0])
	return err
}

// StoredMessage is the subset of a stored message used to dedupe an import.
type StoredMessage struct {
	MsgID    string
	TS       int64
	FromMe   bool
	Text     string
	HasMedia bool
}

// ImportedMessage is one export line ready to insert.
type ImportedMessage struct {
	MsgID      string
	TS         int64
	FromMe     bool
	SenderJID  string
	SenderName string
	Text       string
}

func (d *DB) ChatMessagesForImport(chatJID string) ([]StoredMessage, error) {
	rows, err := d.sql.Query(`SELECT msg_id, ts, from_me,
		COALESCE(NULLIF(text,''), media_caption, ''), COALESCE(media_type,'') <> ''
		FROM messages WHERE chat_jid = ? AND reaction_emoji IS NULL ORDER BY ts, rowid`, chatJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.MsgID, &m.TS, &m.FromMe, &m.Text, &m.HasMedia); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InsertImportedMessages inserts in one transaction and skips IDs already
// present, so re-running an import is a no-op. It returns rows inserted.
func (d *DB) InsertImportedMessages(chatJID, chatName string, msgs []ImportedMessage) (int, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO messages
		(chat_jid, chat_name, msg_id, sender_jid, sender_name, ts, from_me, text)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	inserted := 0
	for _, m := range msgs {
		res, err := stmt.Exec(chatJID, nullString(chatName), m.MsgID, nullString(m.SenderJID),
			nullString(m.SenderName), m.TS, boolToInt64(m.FromMe), m.Text)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

// DeleteImportedMessages removes every imported row in a chat (all chats when
// chatJID is empty) and returns how many were removed.
func (d *DB) DeleteImportedMessages(chatJID string) (int64, error) {
	var res sql.Result
	var err error
	if chatJID == "" {
		res, err = d.sql.Exec(`DELETE FROM messages WHERE msg_id LIKE 'import-%'`)
	} else {
		res, err = d.sql.Exec(`DELETE FROM messages WHERE chat_jid = ? AND msg_id LIKE 'import-%'`, chatJID)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
