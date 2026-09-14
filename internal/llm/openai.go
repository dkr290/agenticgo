package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/dkr290/agenticgo/internal/logger"
)

// OpenAIProvider talks to any OpenAI-compatible chat-completions endpoint
// (Ollama, LM Studio, LocalAI, vLLM, OpenAI itself) through the official
// OpenAI Go SDK. The endpoint is selected with a base-URL override, so any
// server implementing the OpenAI wire format works.
//
// The original hand-rolled SSE client is kept for reference in
// openai.go.bak (not compiled).
type OpenAIProvider struct {
	baseURL string
	apiKey  string
	model   string
	client  openai.Client
	log     logger.Logger
}

// NewOpenAI creates a provider for an OpenAI-compatible endpoint. Local
// servers ignore the API key, but the SDK requires a non-empty one, so an
// empty key is replaced with a placeholder.
func NewOpenAI(baseURL, apiKey, model string) *OpenAIProvider {
	baseURL = strings.TrimSuffix(baseURL, "/")
	key := apiKey
	if key == "" {
		key = "unused" // required by the SDK, ignored by local servers
	}
	return &OpenAIProvider{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		client: openai.NewClient(
			option.WithBaseURL(baseURL),
			option.WithAPIKey(key),
		),
		log: logger.Nop(),
	}
}

// SetLogger wires verbose debug logging into the provider (nil keeps a no-op
// logger). The API key is never logged — only a redacted tail.
func (p *OpenAIProvider) SetLogger(l logger.Logger) {
	if l == nil {
		l = logger.Nop()
	}
	p.log = l
}

// Name returns the provider name.
func (p *OpenAIProvider) Name() string { return "openai-compatible" }

// Model returns the configured model identifier.
func (p *OpenAIProvider) Model() string { return p.model }

// redactKey masks the API key for logs, showing only the last 4 characters.
func redactKey(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

// ListModels queries the provider's /models endpoint and returns model IDs.
// It works against any OpenAI-compatible server (Ollama, LM Studio, vLLM, OpenAI).
func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	p.log.Debug("llm: GET /models", "base_url", p.baseURL, "api_key", redactKey(p.apiKey))
	page, err := p.client.Models.List(ctx)
	if err != nil {
		p.log.Debug("llm: GET /models failed", "error", err)
		return nil, fmt.Errorf("list models: %w", err)
	}
	models := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	p.log.Debug("llm: GET /models ok", "models", len(models))
	return models, nil
}

// ChatCompletion implements Provider.
func (p *OpenAIProvider) ChatCompletion(ctx context.Context, req ChatRequest, onDelta StreamFunc) (Message, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(model),
		Messages: toOAIMessages(req.Messages),
		Tools:    toOAITools(req.Tools),
	}
	if req.Temperature != nil {
		params.Temperature = openai.Float(*req.Temperature)
	}
	if req.MaxTokens != nil {
		params.MaxTokens = openai.Int(int64(*req.MaxTokens))
	}
	p.log.Debug("llm: chat completion",
		"base_url", p.baseURL, "model", model, "stream", req.Stream,
		"messages", len(req.Messages), "tools", len(req.Tools))

	if !req.Stream {
		return p.nonStream(ctx, params, model, onDelta)
	}
	return p.stream(ctx, params, model, onDelta)
}

// nonStream handles a regular chat completion.
func (p *OpenAIProvider) nonStream(ctx context.Context, params openai.ChatCompletionNewParams, model string, onDelta StreamFunc) (Message, error) {
	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		p.log.Debug("llm: request failed", "model", model, "error", err)
		return Message{}, fmt.Errorf("llm request: %w", err)
	}
	if len(resp.Choices) == 0 {
		return Message{}, fmt.Errorf("llm returned no choices")
	}
	msg := Message{Role: RoleAssistant, Content: resp.Choices[0].Message.Content}
	for _, tc := range resp.Choices[0].Message.ToolCalls {
		if tc.Type != "function" {
			continue
		}
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

// stream handles a streaming (SSE) chat completion. The SDK accumulator
// reassembles streamed tool calls by their wire index, so parallel tool
// calls come out correctly separated.
func (p *OpenAIProvider) stream(ctx context.Context, params openai.ChatCompletionNewParams, model string, onDelta StreamFunc) (Message, error) {
	s := p.client.Chat.Completions.NewStreaming(ctx, params)
	defer s.Close()

	var (
		acc       openai.ChatCompletionAccumulator
		assistant strings.Builder
	)
	for s.Next() {
		chunk := s.Current()
		acc.AddChunk(chunk)
		if len(chunk.Choices) == 0 {
			// Some providers send a mid-stream error object instead of
			// choices; surface it instead of ending with an empty reply.
			if msg, ok := streamError(chunk.RawJSON()); ok {
				return Message{}, fmt.Errorf("llm stream error: %s", msg)
			}
			continue
		}
		if delta := chunk.Choices[0].Delta.Content; delta != "" {
			assistant.WriteString(delta)
			if err := onDelta(Delta{Content: delta}); err != nil {
				return Message{}, err
			}
		}
	}
	if err := s.Err(); err != nil {
		p.log.Debug("llm: stream failed", "model", model, "error", err)
		return Message{}, fmt.Errorf("read stream: %w", err)
	}

	msg := Message{Role: RoleAssistant, Content: assistant.String()}
	if len(acc.Choices) > 0 {
		for i, tc := range acc.Choices[0].Message.ToolCalls {
			if tc.Type != "function" {
				continue
			}
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        id,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	if err := onDelta(Delta{ToolCalls: msg.ToolCalls, Done: true}); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// streamError detects a provider error payload sent as an SSE data frame
// ({"error": {...}}) instead of an HTTP error status.
func streamError(raw string) (string, bool) {
	if !strings.Contains(raw, `"error"`) {
		return "", false
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &e); err != nil || e.Error.Message == "" {
		return "", false
	}
	return e.Error.Message, true
}

// toOAIMessages maps our messages to SDK params. A user message with images
// becomes an array of content parts (text + one image_url per image), the
// OpenAI vision shape.
func toOAIMessages(in []Message) []openai.ChatCompletionMessageParamUnion {
	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(in))
	for _, m := range in {
		switch m.Role {
		case RoleSystem:
			msgs = append(msgs, openai.SystemMessage(m.Content))
		case RoleUser:
			if len(m.Images) == 0 {
				msgs = append(msgs, openai.UserMessage(m.Content))
				continue
			}
			parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(m.Images)+1)
			if m.Content != "" {
				parts = append(parts, openai.TextContentPart(m.Content))
			}
			for _, img := range m.Images {
				parts = append(parts, openai.ImageContentPart(
					openai.ChatCompletionContentPartImageImageURLParam{URL: img}))
			}
			msgs = append(msgs, openai.UserMessage(parts))
		case RoleAssistant:
			am := openai.AssistantMessage(m.Content)
			if len(m.ToolCalls) > 0 {
				tcs := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					tcs = append(tcs, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: tc.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: tc.Arguments,
							},
						},
					})
				}
				am.OfAssistant.ToolCalls = tcs
			}
			msgs = append(msgs, am)
		case RoleTool:
			msgs = append(msgs, openai.ToolMessage(m.Content, m.ToolCallID))
		}
	}
	return msgs
}

// toOAITools maps our tool specs to SDK function-tool params.
func toOAITools(specs []ToolSpec) []openai.ChatCompletionToolUnionParam {
	if len(specs) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(specs))
	for _, s := range specs {
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        s.Function.Name,
			Description: openai.String(s.Function.Description),
			Parameters:  shared.FunctionParameters(s.Function.Parameters),
		}))
	}
	return out
}
