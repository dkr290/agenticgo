package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A user message carrying images must serialize as OpenAI content parts:
// a text part plus one image_url part per image. Text-only messages stay
// plain strings.
func TestMultimodalRequestEncoding(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "", "vision-model")
	req := ChatRequest{
		Model: "vision-model",
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "what is this?", Images: []string{"data:image/png;base64,AAA"}},
		},
		Stream: false,
	}
	if _, err := p.ChatCompletion(context.Background(), req, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("request not JSON: %v", err)
	}
	msgs := body["messages"].([]any)

	// System message stays a plain string.
	if msgs[0].(map[string]any)["content"] != "sys" {
		t.Errorf("system content should be a string, got %v", msgs[0])
	}

	// User message becomes an array of parts.
	content, ok := msgs[1].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("user content should be a parts array, got %T", msgs[1].(map[string]any)["content"])
	}
	if len(content) != 2 {
		t.Fatalf("want 2 parts (text+image), got %d", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Errorf("first part should be text, got %v", content[0])
	}
	img := content[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("second part should be image_url, got %v", img)
	}
	url := img["image_url"].(map[string]any)["url"]
	if !strings.HasPrefix(url.(string), "data:image/png;base64,") {
		t.Errorf("image_url should be a data-URL, got %v", url)
	}
}

// A message with no images must keep the plain-string content shape (no
// regression for text-only chats / tool flows).
func TestTextOnlyRequestEncoding(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "", "m")
	req := ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Stream:   false,
	}
	if _, err := p.ChatCompletion(context.Background(), req, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	if body["messages"].([]any)[0].(map[string]any)["content"] != "hello" {
		t.Errorf("text-only content should be a plain string, got %v", body["messages"])
	}
}
