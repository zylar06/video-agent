package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zylar06/video-agent/internal/domain"
)

// Conversations are persisted so a reloaded page — or a restarted service —
// resumes instead of losing context.
//
// Two shapes are stored, because they answer different questions:
//
//   - session_messages holds the UI transcript (domain.View), appended one turn
//     at a time so an interrupted conversation keeps everything already shown.
//   - session_state holds the model-facing message list, which is what actually
//     resumes a conversation. It cannot be derived from the view: an assistant
//     turn that requested tools carries tool_call ids the view never displays,
//     and providers reject tool results whose originating call is absent.
//
// Rewriting state once per call is acceptable because it is small next to the
// transcripts it summarizes.

const timeLayout = time.RFC3339Nano

// SessionInfo describes one stored conversation for a sidebar.
type SessionInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  int       `json:"messages"`
}

// CreateSession registers a conversation. Re-creating an existing id is a no-op
// so a client resuming from localStorage can safely re-announce its session.
func (s *Store) CreateSession(id, title string) error {
	if id == "" {
		return errors.New("session id is required")
	}
	now := time.Now().UTC().Format(timeLayout)
	_, err := s.db.Exec(`INSERT INTO sessions(id,title,created_at,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(id) DO NOTHING`, id, title, now, now)
	return err
}

// AppendSessionMessage stores one transcript entry and, when the user's first
// request arrives, names the conversation after it.
func (s *Store) AppendSessionMessage(sessionID string, message domain.View) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var next int
	if err := tx.QueryRow("SELECT COALESCE(MAX(seq),0)+1 FROM session_messages WHERE session_id=?", sessionID).Scan(&next); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO session_messages(session_id,seq,body) VALUES(?,?,?)", sessionID, next, b); err != nil {
		return err
	}
	titles := message.Role == "user" && message.Text != ""
	if _, err := tx.Exec(`UPDATE sessions SET updated_at=?,
		title=CASE WHEN title='' AND ?=1 THEN ? ELSE title END WHERE id=?`,
		time.Now().UTC().Format(timeLayout), boolToInt(titles), truncateRunes(message.Text, 40), sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveSessionState stores the model-facing message list that resumes a session.
func (s *Store) SaveSessionState(sessionID string, messages []domain.Message) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	b, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO session_state(session_id,body) VALUES(?,?)
		ON CONFLICT(session_id) DO UPDATE SET body=excluded.body`, sessionID, b)
	return err
}

// SessionState returns the stored model-facing messages. A conversation with no
// saved state yet returns nil rather than an error.
func (s *Store) SessionState(sessionID string) ([]domain.Message, error) {
	var b []byte
	err := s.db.QueryRow("SELECT body FROM session_state WHERE session_id=?", sessionID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []domain.Message
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SessionMessages returns the UI transcript in order.
func (s *Store) SessionMessages(sessionID string) ([]domain.View, error) {
	rows, err := s.db.Query("SELECT body FROM session_messages WHERE session_id=? ORDER BY seq", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.View{}
	for rows.Next() {
		var b []byte
		var v domain.View
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Sessions lists stored conversations, most recently updated first.
func (s *Store) Sessions() ([]SessionInfo, error) {
	rows, err := s.db.Query(`SELECT s.id, s.title, s.created_at, s.updated_at,
		(SELECT COUNT(*) FROM session_messages m WHERE m.session_id=s.id)
		FROM sessions s ORDER BY s.updated_at DESC, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionInfo{}
	for rows.Next() {
		var info SessionInfo
		var created, updated string
		if err := rows.Scan(&info.ID, &info.Title, &created, &updated, &info.Messages); err != nil {
			return nil, err
		}
		// A malformed timestamp must not hide the conversation from the user.
		info.CreatedAt, _ = time.Parse(timeLayout, created)
		info.UpdatedAt, _ = time.Parse(timeLayout, updated)
		out = append(out, info)
	}
	return out, rows.Err()
}

// DeleteSession removes a conversation, its transcript and its saved state.
func (s *Store) DeleteSession(id string) error {
	for _, stmt := range []string{
		"DELETE FROM session_messages WHERE session_id=?",
		"DELETE FROM session_state WHERE session_id=?",
		"DELETE FROM sessions WHERE id=?",
	} {
		if _, err := s.db.Exec(stmt, id); err != nil {
			return err
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// truncateRunes shortens a session title without splitting a multi-byte rune.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
