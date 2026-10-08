// openai.go is the OpenAI-compatible provider configuration carrier. The
// agent loop runs on the Microsoft Agent Framework now — this type exists to
// carry endpoint credentials (base URL, API key, model) from the named
// providers store to the engine's MAF client construction
// (RequestOptions), and to answer TestConnection/ListModels for the
// Providers UI. The old hand-rolled streaming ChatCompletion implementation
// was removed when the engine moved to MAF.
package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/dkr290/agenticgo/internal/logger"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// OpenAIProvider carries one OpenAI-compatible endpoint configuration
// (Ollama / LM Studio / vLLM / OpenAI / Azure Foundry's OpenAI-compatible
// endpoint).
type OpenAIProvider struct {
	baseURL string
	apiKey  string
	model   string
	client  openai.Client // used only for ListModels (Providers UI test)
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

// Name implements Provider.
func (p *OpenAIProvider) Name() string { return "openai-compatible" }

// Model implements Provider.
func (p *OpenAIProvider) Model() string { return p.model }

// RequestOptions returns the client options the engine uses to build its MAF
// openai.Client with the same endpoint and credentials. The key placeholder
// logic matches NewOpenAI (the SDK requires a non-empty key).
func (p *OpenAIProvider) RequestOptions() []option.RequestOption {
	key := p.apiKey
	if key == "" {
		key = "unused" // required by the SDK, ignored by local servers
	}
	return []option.RequestOption{
		option.WithBaseURL(p.baseURL),
		option.WithAPIKey(key),
	}
}

// ListModels lists the model IDs the endpoint advertises (Providers UI
// test-connection button).
func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	models := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		models = append(models, m.ID)
	}
	return models, nil
}
