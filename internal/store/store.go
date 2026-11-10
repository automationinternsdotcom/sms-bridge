// Package store keeps captured messages in a local SQLite database.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS conversations (
	conv_id      TEXT PRIMARY KEY,
	name         TEXT NOT NULL DEFAULT '',
	conv_type    TEXT NOT NULL DEFAULT '',
	is_group     INTEGER NOT NULL DEFAULT 0,
	folder       TEXT NOT NULL DEFAULT '',
	participants TEXT NOT NULL DEFAULT '[]',
	last_msg_us  INTEGER NOT NULL DEFAULT 0,
	synced_to_us INTEGER NOT NULL DEFAULT 0,
	backfill_done INTEGER NOT NULL DEFAULT 0,
	backfill_cursor_id TEXT NOT NULL DEFAULT '',
	backfill_cursor_ms INTEGER NOT NULL DEFAULT 0,
	total_reported INTEGER NOT NULL DEFAULT 0,
	updated_at   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
	row_key      TEXT PRIMARY KEY,         -- pairing_id|message_id (|ts if an ID was reused)
	pairing_id   TEXT NOT NULL,
	message_id   TEXT NOT NULL,
	conv_id      TEXT NOT NULL,
	ts_us        INTEGER NOT NULL,
	direction    TEXT NOT NULL,            -- in | out | system
	sender_id    TEXT NOT NULL DEFAULT '',
	sender_name  TEXT NOT NULL DEFAULT '',
	sender_number TEXT NOT NULL DEFAULT '',
	body         TEXT NOT NULL DEFAULT '',
	attachments  TEXT NOT NULL DEFAULT '',
	msg_kind     TEXT NOT NULL DEFAULT '', -- SMS | MMS | RCS
	status       INTEGER NOT NULL DEFAULT 0,
	status_name  TEXT NOT NULL DEFAULT '',
	raw          BLOB,
	first_seen   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	pushed_at    INTEGER                   -- NULL until written to the Sheet
);
CREATE INDEX IF NOT EXISTS messages_conv_ts ON messages(conv_id, ts_us);
CREATE INDEX IF NOT EXISTS messages_msgid ON messages(message_id, conv_id);
CREATE INDEX IF NOT EXISTS messages_unpushed ON messages(pushed_at) WHERE pushed_at IS NULL;
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) GetMeta(key string) string {
	var v string
	_ = s.DB.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

type Conversation struct {
	ConvID, Name, ConvType, Folder, Participants string
	IsGroup                                      bool
	LastMsgUS                                    int64
	SyncedToUS                                   int64
	BackfillDone                                 bool
	BackfillCursorID                             string
	BackfillCursorMS                             int64
	TotalReported                                int64
}

func (s *Store) GetConversation(id string) (*Conversation, error) {
	c := &Conversation{ConvID: id}
	var isGroup, done int
	err := s.DB.QueryRow(`SELECT name,conv_type,is_group,folder,participants,last_msg_us,synced_to_us,backfill_done,backfill_cursor_id,backfill_cursor_ms,total_reported FROM conversations WHERE conv_id=?`, id).
		Scan(&c.Name, &c.ConvType, &isGroup, &c.Folder, &c.Participants, &c.LastMsgUS, &c.SyncedToUS, &done, &c.BackfillCursorID, &c.BackfillCursorMS, &c.TotalReported)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	c.IsGroup, c.BackfillDone = isGroup == 1, done == 1
	return c, err
}

// UpsertConversationInfo updates descriptive fields only, never sync progress.
func (s *Store) UpsertConversationInfo(c *Conversation) error {
	_, err := s.DB.Exec(`INSERT INTO conversations(conv_id,name,conv_type,is_group,folder,participants,last_msg_us,updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(conv_id) DO UPDATE SET name=excluded.name, conv_type=excluded.conv_type, is_group=excluded.is_group,
		folder=excluded.folder, participants=excluded.participants, last_msg_us=excluded.last_msg_us, updated_at=excluded.updated_at`,
		c.ConvID, c.Name, c.ConvType, b2i(c.IsGroup), c.Folder, c.Participants, c.LastMsgUS, time.Now().Unix())
	return err
}

func (s *Store) SetSyncedTo(convID string, us int64) error {
	_, err := s.DB.Exec(`UPDATE conversations SET synced_to_us=MAX(synced_to_us,?) WHERE conv_id=?`, us, convID)
	return err
}

func (s *Store) SetBackfill(convID string, done bool, cursorID string, cursorMS, total int64) error {
	_, err := s.DB.Exec(`UPDATE conversations SET backfill_done=?, backfill_cursor_id=?, backfill_cursor_ms=?, total_reported=MAX(total_reported,?) WHERE conv_id=?`,
		b2i(done), cursorID, cursorMS, total, convID)
	return err
}

type Message struct {
	PairingID, MessageID, ConvID                  string
	TsUS                                          int64
	Direction, SenderID, SenderName, SenderNumber string
	Body, Attachments, Kind                       string
	Status                                        int
	StatusName                                    string
	Raw                                           []byte
}

// Upsert stores a message. It returns isNew=true when a row was inserted.
// The same ID with a changed status or body updates the row in place. A reused
// ID (different conversation or timestamp) is stored as a separate row.
func (s *Store) Upsert(m *Message) (isNew bool, err error) {
	key := m.PairingID + "|" + m.MessageID
	// Same message seen under any pairing (e.g. after a re-pair): reuse its row.
	var existingKey string
	err = s.DB.QueryRow(`SELECT row_key FROM messages WHERE message_id=? AND conv_id=? AND ABS(ts_us-?)<=600000000 LIMIT 1`,
		m.MessageID, m.ConvID, m.TsUS).Scan(&existingKey)
	switch {
	case err == nil:
		key = existingKey
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	default:
		var one int
		if s.DB.QueryRow(`SELECT 1 FROM messages WHERE row_key=?`, key).Scan(&one) == nil {
			// The ID was reused for a different message: keep both.
			key = fmt.Sprintf("%s|%d", key, m.TsUS)
		}
	}
	now := time.Now().Unix()
	res, err := s.DB.Exec(`INSERT INTO messages(row_key,pairing_id,message_id,conv_id,ts_us,direction,sender_id,sender_name,sender_number,body,attachments,msg_kind,status,status_name,raw,first_seen,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(row_key) DO NOTHING`,
		key, m.PairingID, m.MessageID, m.ConvID, m.TsUS, m.Direction, m.SenderID, m.SenderName, m.SenderNumber,
		m.Body, m.Attachments, m.Kind, m.Status, m.StatusName, m.Raw, now, now)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	// Already in the Sheet: only track status (content may have been scrubbed).
	if _, err = s.DB.Exec(`UPDATE messages SET status=?, status_name=?, updated_at=? WHERE row_key=? AND pushed_at IS NOT NULL AND status<>?`,
		m.Status, m.StatusName, now, key, m.Status); err != nil {
		return false, err
	}
	// Not yet pushed: refresh mutable fields. Keep the body if the update is empty.
	_, err = s.DB.Exec(`UPDATE messages SET status=?, status_name=?, raw=?, updated_at=?,
		body=CASE WHEN ?<>'' THEN ? ELSE body END,
		attachments=CASE WHEN ?<>'' THEN ? ELSE attachments END,
		sender_name=CASE WHEN ?<>'' THEN ? ELSE sender_name END,
		sender_number=CASE WHEN ?<>'' THEN ? ELSE sender_number END
		WHERE row_key=? AND pushed_at IS NULL AND (status<>? OR body<>? OR attachments<>?)`,
		m.Status, m.StatusName, m.Raw, now, m.Body, m.Body, m.Attachments, m.Attachments,
		m.SenderName, m.SenderName, m.SenderNumber, m.SenderNumber, key, m.Status, m.Body, m.Attachments)
	return false, err
}

// Known reports whether a message ID is already stored for the conversation (any pairing).
func (s *Store) Known(convID, messageID string) bool {
	var one int
	return s.DB.QueryRow(`SELECT 1 FROM messages WHERE message_id=? AND conv_id=? LIMIT 1`, messageID, convID).Scan(&one) == nil
}

type Row struct {
	RowKey                                           string
	TsUS                                             int64
	Direction, Contact, Number, ConvName, Kind       string
	Body, Attachments, StatusName, ConvID, MessageID string
}

// PushCandidates returns unpushed real messages old enough to be final.
func (s *Store) PushCandidates(minAge time.Duration, limit int) ([]Row, error) {
	cutoff := time.Now().Add(-minAge).UnixMicro()
	rows, err := s.DB.Query(`SELECT m.row_key, m.ts_us, m.direction, m.sender_name, m.sender_number, COALESCE(c.name,''), m.msg_kind,
		m.body, m.attachments, m.status_name, m.conv_id, m.message_id
		FROM messages m LEFT JOIN conversations c ON c.conv_id=m.conv_id
		WHERE m.pushed_at IS NULL AND m.direction IN ('in','out') AND m.ts_us <= ?
		AND (m.body<>'' OR m.attachments<>'' OR m.first_seen < strftime('%s','now')-600) -- let MMS content arrive
		AND NOT (m.status IN (101,102,103,104,105,115) AND m.first_seen > strftime('%s','now')-86400) -- media still downloading
		AND m.status NOT IN (3,4,5,6,7,10,16,20)  -- draft, queued, sending, scheduled, validating
		ORDER BY m.ts_us LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.RowKey, &r.TsUS, &r.Direction, &r.Contact, &r.Number, &r.ConvName, &r.Kind,
			&r.Body, &r.Attachments, &r.StatusName, &r.ConvID, &r.MessageID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScrubPushed erases message content (text, attachments, sender name/number,
// raw protobuf) from rows already written to the Sheet, and from system notices
// that are never pushed. IDs and timestamps stay for dedup and resume.
func (s *Store) ScrubPushed() (int64, error) {
	res, err := s.DB.Exec(`UPDATE messages SET body='', attachments='', sender_name='', sender_number='', raw=NULL
		WHERE (pushed_at IS NOT NULL OR direction='system' OR first_seen < strftime('%s','now')-7*86400)
		AND (body<>'' OR attachments<>'' OR sender_name<>'' OR sender_number<>'' OR raw IS NOT NULL)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		_, _ = s.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	return n, nil
}

func (s *Store) MarkPushed(keys []string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, k := range keys {
		if _, err := tx.Exec(`UPDATE messages SET pushed_at=? WHERE row_key=?`, now, k); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

type Stats struct {
	Messages, Unpushed, Conversations, BackfillPending int
	ByKind                                             map[string]int
	ByDirection                                        map[string]int
}

func (s *Store) Stats() (*Stats, error) {
	st := &Stats{ByKind: map[string]int{}, ByDirection: map[string]int{}}
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&st.Messages)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE pushed_at IS NULL AND direction IN ('in','out')`).Scan(&st.Unpushed)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&st.Conversations)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM conversations WHERE backfill_done=0`).Scan(&st.BackfillPending)
	for q, m := range map[string]map[string]int{
		`SELECT msg_kind, COUNT(*) FROM messages GROUP BY msg_kind`:   st.ByKind,
		`SELECT direction, COUNT(*) FROM messages GROUP BY direction`: st.ByDirection,
	} {
		rows, err := s.DB.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			var n int
			_ = rows.Scan(&k, &n)
			m[k] = n
		}
		rows.Close()
	}
	return st, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
