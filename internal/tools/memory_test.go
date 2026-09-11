package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fakeDocs is an in-memory doc store keyed by id, used to exercise the
// search_docs / read_doc tools the way the engine wires them.
func fakeDocs() (DocSearcher, DocReader) {
	type doc struct{ title, content string }
	docs := map[int64]doc{
		1: {"Payments runbook", "Scale payments-api to 2 replicas on restart."},
	}
	search := func(_ context.Context, query string, limit int) ([]string, error) {
		var out []string
		for id, d := range docs {
			if strings.Contains(strings.ToLower(d.title), strings.ToLower(query)) {
				out = append(out, fmt.Sprintf("%d — %s", id, d.title))
			}
		}
		return out, nil
	}
	read := func(_ context.Context, id int64) (string, string, error) {
		d, ok := docs[id]
		if !ok {
			return "", "", fmt.Errorf("document %d not found", id)
		}
		return d.title, d.content, nil
	}
	return search, read
}

func TestSearchDocsReturnsIDAndTitle(t *testing.T) {
	search, _ := fakeDocs()
	out, err := NewSearchDocs(search).Call(context.Background(), json.RawMessage(`{"query":"payments"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 — Payments runbook") {
		t.Errorf("search_docs should return 'id — title', got: %q", out)
	}
}

func TestSearchDocsNoMatch(t *testing.T) {
	search, _ := fakeDocs()
	out, err := NewSearchDocs(search).Call(context.Background(), json.RawMessage(`{"query":"nonexistent"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "No documents matched." {
		t.Errorf("got %q", out)
	}
}

func TestReadDocReturnsContent(t *testing.T) {
	_, read := fakeDocs()
	out, err := NewReadDoc(read).Call(context.Background(), json.RawMessage(`{"id":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Scale payments-api to 2 replicas") {
		t.Errorf("read_doc should return content, got: %q", out)
	}
	if !strings.Contains(out, "Payments runbook") {
		t.Errorf("read_doc should include the title, got: %q", out)
	}
}

func TestReadDocUnknownID(t *testing.T) {
	_, read := fakeDocs()
	// Unknown id (e.g. another agent's doc id) must error, not leak content.
	if _, err := NewReadDoc(read).Call(context.Background(), json.RawMessage(`{"id":99}`)); err == nil {
		t.Error("read_doc with unknown id should error")
	}
}

func TestReadDocBadID(t *testing.T) {
	_, read := fakeDocs()
	if _, err := NewReadDoc(read).Call(context.Background(), json.RawMessage(`{"id":0}`)); err == nil {
		t.Error("read_doc with id 0 should error")
	}
}
