// Package store persists conversations and agent knowledge in SQLite.
// It uses modernc.org/sqlite so no cgo is required (k8s / static binary friendly).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Reasonable defaults for a single-process embedded DB.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    agent      TEXT    NOT NULL DEFAULT 'default',
    session    TEXT    NOT NULL,
    role       TEXT    NOT NULL,
    content    TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(agent, session, id);

CREATE TABLE IF NOT EXISTS knowledge (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    agent      TEXT    NOT NULL DEFAULT 'default',
    content    TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_knowledge_agent ON knowledge(agent, id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Idempotent column additions for DBs created before per-agent scoping.
	s.addColumnIfMissing("messages", "agent", `ALTER TABLE messages ADD COLUMN agent TEXT NOT NULL DEFAULT 'default'`)
	s.addColumnIfMissing("knowledge", "agent", `ALTER TABLE knowledge ADD COLUMN agent TEXT NOT NULL DEFAULT 'default'`)
	return nil
}

func (s *Store) addColumnIfMissing(table, column, alter string) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err == nil && n == 0 {
		_, _ = s.db.Exec(alter)
	}
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// AppendMessage stores a message for an agent in a session.
func (s *Store) AppendMessage(ctx context.Context, agent, session, role, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (agent, session, role, content, created_at) VALUES (?,?,?,?,?)`,
		agent, session, role, content, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// Messages returns the stored messages for an agent+session, oldest first.
func (s *Store) Messages(ctx context.Context, agent, session string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content FROM messages WHERE agent = ? AND session = ? ORDER BY id DESC LIMIT ?`,
		agent, session, limit)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var rev []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		rev = append(rev, m)
	}
	// Reverse to chronological order.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, rows.Err()
}

// AddKnowledge appends a knowledge entry (self-evolution memory) for an agent.
func (s *Store) AddKnowledge(ctx context.Context, agent, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge (agent, content, created_at) VALUES (?,?,?)`,
		agent, content, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("insert knowledge: %w", err)
	}
	return nil
}

// Knowledge returns recent knowledge entries for an agent, oldest first.
func (s *Store) Knowledge(ctx context.Context, agent string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT content FROM knowledge WHERE agent = ? ORDER BY id DESC LIMIT ?`, agent, limit)
	if err != nil {
		return nil, fmt.Errorf("query knowledge: %w", err)
	}
	defer rows.Close()

	var rev []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan knowledge: %w", err)
		}
		rev = append(rev, c)
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, rows.Err()
}

// Message is a stored conversation turn.
type Message struct {
	Role    string
	Content string
}
