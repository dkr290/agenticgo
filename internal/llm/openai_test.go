package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3"
)

// chatServer 429s the first failTimes requests (Retry-After: 0 keeps the test
// fast) and then answers a minimal chat completion. attempts counts hits.
func chatServer(t *testing.T, failTimes int32, attempts *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= failTimes {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited","type":"tokens","code":"rate_limit_exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
}

func chatOnce(p *OpenAIProvider) (string, error) {
	client := openai.NewClient(p.RequestOptions()...)
	resp, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "m",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	})
	if err != nil {
		return "", err
	}
	return resp.Choices[0].Message.Content, nil
}

// A configured retry budget rides out transient 429s: 2 failures + 1 success
// = 3 attempts, and the call succeeds instead of erroring into the chat.
func TestRequestOptionsMaxRetriesRidesOut429(t *testing.T) {
	var attempts atomic.Int32
	srv := chatServer(t, 2, &attempts)
	defer srv.Close()

	p := NewOpenAI(srv.URL+"/v1", "k", "m")
	p.SetMaxRetries(4)

	got, err := chatOnce(p)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got != "ok" {
		t.Fatalf("content = %q, want ok", got)
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

// Without the knob the SDK default (2 retries = 3 attempts) applies and the
// 429 surfaces as an error once the budget is spent.
func TestRequestOptionsZeroKeepsSDKDefault(t *testing.T) {
	var attempts atomic.Int32
	srv := chatServer(t, 10, &attempts) // never stops failing
	defer srv.Close()

	p := NewOpenAI(srv.URL+"/v1", "k", "m")
	// no SetMaxRetries: 0 keeps the SDK default of 2 retries
	if _, err := chatOnce(p); err == nil {
		t.Fatal("expected the 429 to surface as an error")
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("attempts = %d, want 3 (1 request + 2 SDK-default retries)", n)
	}
}
