// Package store persists conversations and agent knowledge in SQLite.
// It uses modernc.org/sqlite so no cgo is required (k8s / static binary friendly).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

-- Observations are high-churn, timestamped facts captured by recurring agents
-- (e.g. a cron job watching a k8s cluster). Unlike curated knowledge, they are
-- time-bound: old observations are pruned and only the latest is injected into
-- prompts, so stale state does not mislead the model.
CREATE TABLE IF NOT EXISTS observations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    agent      TEXT    NOT NULL DEFAULT 'default',
    content    TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_observations_agent ON observations(agent, id);

-- Full-text index over curated knowledge (FTS5 is bundled with SQLite).
CREATE VIRTUAL TABLE IF NOT EXISTS knowledge_fts USING fts5(content, agent UNINDEXED);

-- Keep the FTS index in sync with the knowledge table.
CREATE TRIGGER IF NOT EXISTS knowledge_ai AFTER INSERT ON knowledge BEGIN
    INSERT INTO knowledge_fts(rowid, content, agent) VALUES (new.id, new.content, new.agent);
END;
CREATE TRIGGER IF NOT EXISTS knowledge_ad AFTER DELETE ON knowledge BEGIN
    INSERT INTO knowledge_fts(knowledge_fts, rowid, content, agent) VALUES('delete', old.id, old.content, old.agent);
END;

-- Knowledge-base documents: user-uploaded reference text, full-text indexed
-- over the *content* (so documents actually feed agent recall).
CREATE TABLE IF NOT EXISTS knowledge_docs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    agent      TEXT    NOT NULL DEFAULT 'default',
    title      TEXT    NOT NULL,
    content    TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kdocs_agent ON knowledge_docs(agent, id);
CREATE VIRTUAL TABLE IF NOT EXISTS knowledge_docs_fts USING fts5(title, content, agent UNINDEXED);
CREATE TRIGGER IF NOT EXISTS kdocs_ai AFTER INSERT ON knowledge_docs BEGIN
    INSERT INTO knowledge_docs_fts(rowid, title, content, agent) VALUES (new.id, new.title, new.content, new.agent);
END;
CREATE TRIGGER IF NOT EXISTS kdocs_ad AFTER DELETE ON knowledge_docs BEGIN
    INSERT INTO knowledge_docs_fts(knowledge_docs_fts, rowid, title, content, agent) VALUES('delete', old.id, old.title, old.content, old.agent);
END;
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Idempotent column additions for DBs created before per-agent scoping.
	s.addColumnIfMissing("messages", "agent", `ALTER TABLE messages ADD COLUMN agent TEXT NOT NULL DEFAULT 'default'`)
	s.addColumnIfMissing("knowledge", "agent", `ALTER TABLE knowledge ADD COLUMN agent TEXT NOT NULL DEFAULT 'default'`)
	// Backfill the FTS index for knowledge rows that predate it.
	if _, err := s.db.Exec(`
		INSERT INTO knowledge_fts(rowid, content, agent)
		SELECT k.id, k.content, k.agent FROM knowledge k
		WHERE NOT EXISTS (SELECT 1 FROM knowledge_fts f WHERE f.rowid = k.id)`); err != nil {
		return fmt.Errorf("backfill knowledge fts: %w", err)
	}
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

// SearchKnowledge runs an FTS5 keyword search over an agent's curated
// knowledge. Returns entries most relevant first (BM25 ranking).
func (s *Store) SearchKnowledge(ctx context.Context, agent, query string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	// Quote each term so arbitrary user input can't break the MATCH parser.
	terms := strings.Fields(q)
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	match := strings.Join(terms, " OR ")
	rows, err := s.db.QueryContext(ctx,
		`SELECT content FROM knowledge_fts WHERE knowledge_fts MATCH ? AND agent = ?
		 ORDER BY rank LIMIT ?`, match, agent, limit)
	if err != nil {
		return nil, fmt.Errorf("search knowledge: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan knowledge: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- Observations (high-churn, time-bound agent findings) ---

// Observation is a single timestamped finding (e.g. a k8s cluster snapshot).
type Observation struct {
	ID        int64  `json:"id"`
	Agent     string `json:"agent"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

// AddObservation records a timestamped observation for an agent.
func (s *Store) AddObservation(ctx context.Context, agent, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO observations (agent, content, created_at) VALUES (?,?,?)`,
		agent, content, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("insert observation: %w", err)
	}
	return nil
}

// LatestObservation returns the most recent observation for an agent, or nil.
func (s *Store) LatestObservation(ctx context.Context, agent string) (*Observation, error) {
	var o Observation
	err := s.db.QueryRowContext(ctx,
		`SELECT id, agent, content, created_at FROM observations
		 WHERE agent = ? ORDER BY id DESC LIMIT 1`, agent).
		Scan(&o.ID, &o.Agent, &o.Content, &o.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest observation: %w", err)
	}
	return &o, nil
}

// ListObservations returns recent observations for an agent, newest first.
func (s *Store) ListObservations(ctx context.Context, agent string, limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent, content, created_at FROM observations
		 WHERE agent = ? ORDER BY id DESC LIMIT ?`, agent, limit)
	if err != nil {
		return nil, fmt.Errorf("list observations: %w", err)
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.Agent, &o.Content, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan observation: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PruneObservations deletes observations older than olderThan (unix seconds)
// and caps the total kept per agent at keepLatest. This is the retention
// control that prevents unbounded accumulation of stale state.
func (s *Store) PruneObservations(ctx context.Context, agent string, olderThan int64, keepLatest int) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM observations WHERE agent = ? AND created_at < ?`, agent, olderThan); err != nil {
		return fmt.Errorf("prune old observations: %w", err)
	}
	if keepLatest > 0 {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM observations WHERE agent = ? AND id NOT IN
			 (SELECT id FROM observations WHERE agent = ? ORDER BY id DESC LIMIT ?)`,
			agent, agent, keepLatest); err != nil {
			return fmt.Errorf("cap observations: %w", err)
		}
	}
	return nil
}

// --- Knowledge-base documents ---

// KnowledgeDoc is a user-uploaded reference document (title + content),
// full-text indexed over content so it feeds agent recall.
type KnowledgeDoc struct {
	ID        int64  `json:"id"`
	Agent     string `json:"agent"`
	Title     string `json:"title"`
	Content   string `json:"content,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// AddKnowledgeDoc stores a document for an agent (indexed for search).
func (s *Store) AddKnowledgeDoc(ctx context.Context, agent, title, content string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_docs (agent, title, content, created_at) VALUES (?,?,?,?)`,
		agent, title, content, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("insert knowledge doc: %w", err)
	}
	return res.LastInsertId()
}

// ListKnowledgeDocs returns an agent's documents (metadata only), newest first.
func (s *Store) ListKnowledgeDocs(ctx context.Context, agent string) ([]KnowledgeDoc, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent, title, created_at FROM knowledge_docs WHERE agent = ? ORDER BY id DESC`, agent)
	if err != nil {
		return nil, fmt.Errorf("list knowledge docs: %w", err)
	}
	defer rows.Close()
	out := []KnowledgeDoc{}
	for rows.Next() {
		var d KnowledgeDoc
		if err := rows.Scan(&d.ID, &d.Agent, &d.Title, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan knowledge doc: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetKnowledgeDoc returns one document with content.
func (s *Store) GetKnowledgeDoc(ctx context.Context, id int64) (*KnowledgeDoc, error) {
	var d KnowledgeDoc
	err := s.db.QueryRowContext(ctx,
		`SELECT id, agent, title, content, created_at FROM knowledge_docs WHERE id = ?`, id).
		Scan(&d.ID, &d.Agent, &d.Title, &d.Content, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("document %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get knowledge doc: %w", err)
	}
	return &d, nil
}

// DeleteKnowledgeDoc removes a document.
func (s *Store) DeleteKnowledgeDoc(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM knowledge_docs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete knowledge doc: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("document %d not found", id)
	}
	return nil
}

// SearchDocs runs an FTS5 search over an agent's documents (title + content).
func (s *Store) SearchDocs(ctx context.Context, agent, query string, limit int) ([]KnowledgeDoc, error) {
	if limit <= 0 {
		limit = 10
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	terms := strings.Fields(q)
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	match := strings.Join(terms, " OR ")
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, title FROM knowledge_docs_fts WHERE knowledge_docs_fts MATCH ? AND agent = ?
		 ORDER BY rank LIMIT ?`, match, agent, limit)
	if err != nil {
		return nil, fmt.Errorf("search docs: %w", err)
	}
	defer rows.Close()
	var out []KnowledgeDoc
	for rows.Next() {
		var d KnowledgeDoc
		if err := rows.Scan(&d.ID, &d.Title); err != nil {
			return nil, fmt.Errorf("scan doc: %w", err)
		}
		d.Agent = agent
		out = append(out, d)
	}
	return out, rows.Err()
}

// Session summarizes one conversation for the UI session picker.
type Session struct {
	Agent     string `json:"agent"`
	Session   string `json:"session"`
	Messages  int    `json:"messages"`
	UpdatedAt int64  `json:"updated_at"`
}

// ListSessions returns all known agent+session pairs, most recent first.
func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent, session, COUNT(*), MAX(created_at)
		 FROM messages GROUP BY agent, session ORDER BY MAX(created_at) DESC`)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.Agent, &sess.Session, &sess.Messages, &sess.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// Message is a stored conversation turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
