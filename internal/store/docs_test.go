package store

import (
	"context"
	"os"
	"testing"
)

// The read_doc tool must never leak one agent's documents to another, even
// when the caller knows the document's ID.
func TestKnowledgeDocAgentScoping(t *testing.T) {
	dir, _ := os.MkdirTemp("", "st")
	defer os.RemoveAll(dir)
	st, err := Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	idA, err := st.AddKnowledgeDoc(ctx, "a1", "Payments runbook", "scale to 2 replicas")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddKnowledgeDoc(ctx, "a2", "Secret a2 doc", "a2 private"); err != nil {
		t.Fatal(err)
	}

	// The owner can read its own document.
	d, err := st.GetKnowledgeDocForAgent(ctx, idA, "a1")
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if d.Content != "scale to 2 replicas" {
		t.Errorf("content = %q", d.Content)
	}

	// Another agent asking for the same ID must get not-found.
	if _, err := st.GetKnowledgeDocForAgent(ctx, idA, "a2"); err == nil {
		t.Error("cross-agent read by id should fail")
	}

	// FTS search is also agent-scoped.
	hits, err := st.SearchDocs(ctx, "a2", "payments", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("a2 should not find a1's doc, got %v", hits)
	}
	hits, err = st.SearchDocs(ctx, "a1", "payments", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != idA {
		t.Errorf("a1 should find its doc, got %v", hits)
	}
}
