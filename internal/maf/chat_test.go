package maf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkr290/agenticgo/internal/tools"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestChatCancellationStopsRemainingBatchAndKeepsResults(t *testing.T) {
	st := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstRan := false
	first := functool.MustNew(functool.Config{Name: "first"}, func(context.Context, struct{}) (string, error) {
		firstRan = true
		cancel() // Cancellation happens during the first call, not before the batch.
		return "first side effect completed", nil
	})
	workspace := t.TempDir()
	write, err := tools.NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"first","arguments":"{}"}},{"index":1,"id":"b","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"after-cancel\",\"content\":\"must not run\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a := NewChatAgent(openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("unused")), ChatConfig{
		Model: "m", MaxIterations: 3, Tools: []tool.Tool{first, write}, History: NewHistoryProvider(st, "demo", "s"),
	})
	_, err = a.RunText(ctx, "two actions", agent.Stream(true)).Collect()
	if !errors.Is(err, context.Canceled) || !firstRan {
		t.Fatalf("firstRan=%v err=%v", firstRan, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "after-cancel")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later write executed: %v", err)
	}
	rows, err := st.Messages(context.Background(), "demo", "s", 40)
	if err != nil || len(rows) != 4 {
		t.Fatalf("cancelled batch history: %+v %v", rows, err)
	}
	if rows[2].Name != "first" || rows[2].Content != "first side effect completed" || rows[3].Name != "write_file" || !strings.Contains(rows[3].Content, "context canceled") {
		t.Fatalf("completed/cancelled results not preserved: %+v", rows)
	}
	history, err := NewHistoryProvider(st, "demo", "s").Invoking(context.Background(), agent.InvokingContext{})
	if err != nil || len(history) != 4 {
		t.Fatalf("cancelled batch is not replayable: %v %v", history, err)
	}
}

func TestChatNonStreamingRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","refusal":"Cannot assist."},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	a := NewChatAgent(openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("unused")), ChatConfig{Model: "m"})
	response, err := a.RunText(context.Background(), "question").Collect()
	if err != nil {
		t.Fatal(err)
	}
	for c := range response.Contents() {
		if refusal, ok := c.(*message.ErrorContent); ok && refusal.Message == "Cannot assist." {
			return
		}
	}
	t.Fatalf("refusal content lost: %+v", response)
}
