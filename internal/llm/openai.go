package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIProvider talks to any OpenAI-compatible /chat/completions endpoint
// (Ollama, LM Studio, vLLM, OpenAI itself). It streams via SSE and parses
// incremental tool calls.
type OpenAIProvider struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewOpenAI creates a provider for an OpenAI-compatible endpoint.
func NewOpenAI(baseURL, apiKey, model string) *OpenAIProvider {
	return &OpenAIProvider{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		client: &http.Client{
			Timeout: 0, // no overall timeout; streaming can be long
		},
	}
}

// Name returns the provider name.
func (p *OpenAIProvider) Name() string { return "openai-compatible" }

// Model returns the configured model identifier.
func (p *OpenAIProvider) Model() string { return p.model }

// ListModels queries the provider's /models endpoint and returns model IDs.
// It works against any OpenAI-compatible server (Ollama, LM Studio, vLLM, OpenAI).
func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slurp, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("list models: %s: %s", resp.Status, strings.TrimSpace(string(slurp)))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

// wire types for the OpenAI-compatible API.
type oaiRequest struct {
	Model       string     `json:"model"`
	Messages    []oaiMsg   `json:"messages"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	MaxTokens   *int       `json:"max_tokens,omitempty"`
	Stream      bool       `json:"stream"`
}

type oaiMsg struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiChunk struct {
	Choices []struct {
		Delta struct {
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// ChatCompletion implements Provider.
func (p *OpenAIProvider) ChatCompletion(ctx context.Context, req ChatRequest, onDelta StreamFunc) (Message, error) {
	msgs := make([]oaiMsg, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := oaiMsg{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			var out oaiToolCall
			out.ID = tc.ID
			out.Type = "function"
			out.Function.Name = tc.Name
			out.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, out)
		}
		msgs = append(msgs, om)
	}

	model := req.Model
	if model == "" {
		model = p.model
	}
	body := oaiRequest{
		Model:       model,
		Messages:    msgs,
		Tools:       req.Tools,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return Message{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return Message{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slurp, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, fmt.Errorf("llm returned %s: %s", resp.Status, strings.TrimSpace(string(slurp)))
	}

	if !req.Stream {
		return parseNonStream(resp.Body, onDelta)
	}

	var (
		assistant strings.Builder
		// Accumulate streaming tool calls by index.
		toolArgs = map[int]*strings.Builder{}
		toolName = map[int]string{}
		toolID   = map[int]string{}
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk oaiChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Tolerate keep-alives / non-JSON lines from quirky providers.
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			assistant.WriteString(delta.Content)
			if err := onDelta(Delta{Content: delta.Content}); err != nil {
				return Message{}, err
			}
		}

		for i, tc := range delta.ToolCalls {
			if tc.ID != "" {
				toolID[i] = tc.ID
			}
			if tc.Function.Name != "" {
				toolName[i] = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				b, ok := toolArgs[i]
				if !ok {
					b = &strings.Builder{}
					toolArgs[i] = b
				}
				b.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Message{}, fmt.Errorf("read stream: %w", err)
	}

	msg := Message{Role: RoleAssistant, Content: assistant.String()}

	// Emit accumulated tool calls (ordered by index).
	if len(toolArgs) > 0 {
		for i := 0; i < len(toolArgs); i++ {
			id := toolID[i]
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        id,
				Name:      toolName[i],
				Arguments: toolArgs[i].String(),
			})
		}
		// Notify the loop that tool calls are ready.
		if err := onDelta(Delta{ToolCalls: msg.ToolCalls, Done: true}); err != nil {
			return Message{}, err
		}
	} else if err := onDelta(Delta{Done: true}); err != nil {
		return Message{}, err
	}

	return msg, nil
}

// parseNonStream handles a regular (non-SSE) chat completion response.
func parseNonStream(body io.Reader, onDelta StreamFunc) (Message, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content   string        `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return Message{}, fmt.Errorf("decode response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return Message{}, fmt.Errorf("llm returned no choices")
	}
	c := resp.Choices[0].Message
	msg := Message{Role: RoleAssistant, Content: c.Content}
	for _, tc := range c.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	if msg.Content != "" {
		if err := onDelta(Delta{Content: msg.Content}); err != nil {
			return Message{}, err
		}
	}
	if err := onDelta(Delta{ToolCalls: msg.ToolCalls, Done: true}); err != nil {
		return Message{}, err
	}
	return msg, nil
}
