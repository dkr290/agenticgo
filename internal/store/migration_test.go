package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyAndBackfillDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
	CREATE TABLE messages(id INTEGER PRIMARY KEY, session TEXT, role TEXT, content TEXT, created_at INTEGER);
	INSERT INTO messages VALUES(1,'old','user','hello',1);
	CREATE TABLE knowledge(id INTEGER PRIMARY KEY, content TEXT, created_at INTEGER);
	INSERT INTO knowledge VALUES(1,'old memory',1);
	CREATE TABLE knowledge_docs(id INTEGER PRIMARY KEY, agent TEXT, title TEXT, content TEXT, created_at INTEGER);
	INSERT INTO knowledge_docs VALUES(1,'demo','runbook','legacy searchable document',1);`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		msgs, err := st.Messages(ctx, "default", "old", 10)
		if err != nil || len(msgs) != 1 {
			st.Close()
			t.Fatalf("legacy messages: %v %v", msgs, err)
		}
		hits, err := st.SearchDocs(ctx, "demo", "legacy", 10)
		if err != nil || len(hits) != 1 {
			st.Close()
			t.Fatalf("legacy docs: %v %v", hits, err)
		}
		if err := st.AppendMessage(ctx, "default", "old", "assistant", "hi"); err != nil {
			st.Close()
			t.Fatal(err)
		}
		if err := st.DeleteSession(ctx, "default", "old"); err != nil {
			st.Close()
			t.Fatal(err)
		}
		if err := st.AppendMessage(ctx, "default", "old", "user", "hello"); err != nil {
			st.Close()
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
