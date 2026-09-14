// Command agenticgo runs the agenticgo server: a simplified, self-hosted
// AI agent gateway with a chat UI, an OpenAI-compatible LLM backend,
// per-agent context files (SOUL.md/AGENTS.md) and skills, allow-listed
// built-in tools, and SQLite-backed memory.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dkr290/agenticgo/internal/agent"
	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
	"github.com/dkr290/agenticgo/internal/crypto"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/logger"
	"github.com/dkr290/agenticgo/internal/providers"
	"github.com/dkr290/agenticgo/internal/scaffold"
	"github.com/dkr290/agenticgo/internal/server"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/dkr290/agenticgo/internal/tools"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Verbose debug logging to stderr when AGENTICGO_DEBUG=true. Wired into
	// the providers/LLM path for now (other packages can adopt it later).
	lg := logger.New(cfg.Debug)
	if cfg.Debug {
		lg.Debug("debug logging enabled")
	}

	// Ensure data + workspace dirs exist.
	if err := os.MkdirAll(cfg.WorkspaceDir, 0o755); err != nil {
		log.Fatalf("workspace: %v", err)
	}

	// Agent registry (one directory per agent under AgentsDir).
	agentReg, err := agents.NewRegistry(cfg.AgentsDir)
	if err != nil {
		log.Fatalf("agents: %v", err)
	}
	// Seed a default agent if none exist so the UI has something to chat with.
	if list, err := agentReg.List(); err == nil && len(list) == 0 {
		if _, err := agentReg.Create("default", "AgenticGo",
			"A helpful general-purpose agent.", "", agents.AgentConfig{}); err != nil {
			log.Printf("seed default agent: %v", err)
		} else {
			log.Printf("seeded default agent at %s/default", cfg.AgentsDir)
		}
	}

	// Persistent store (conversations + knowledge), per-agent scoped.
	st, err := store.Open(filepath.Join(cfg.DataDir, "agenticgo.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Global skills library: skills are uploaded once here and enabled per
	// agent (config.json enabled_skills) — never inherited implicitly.
	if _, err := agentReg.SkillsLibraryDir(); err != nil {
		log.Fatalf("skills library: %v", err)
	}

	// LLM provider (OpenAI-compatible: Ollama / LM Studio / vLLM / OpenAI).
	provider := llm.NewOpenAI(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	provider.SetLogger(lg)

	// Encryption key for provider API keys at rest (env, else a generated
	// data/secret.key file).
	encKey, err := crypto.LoadKey("AGENTICGO_SECRET_KEY", filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		log.Fatalf("secret key: %v", err)
	}

	// Named provider store (editable from the Providers UI). Seeded from env.
	// API keys are encrypted at rest.
	providerStore, err := providers.Open(filepath.Join(cfg.DataDir, "providers.json"), encKey)
	if err != nil {
		log.Fatalf("providers: %v", err)
	}
	providerStore.SetLogger(lg)
	providerStore.SeedDefault(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)

	// Scaffolding for not-yet-implemented features (MCP servers, cron).
	scaff := scaffold.New()

	// Tool registry with built-ins, gated by the tool allow-list.
	reg := tools.NewRegistry(cfg.ToolAllowList)
	mustRegister(reg, func() (tools.Tool, error) { return tools.NewReadFile(cfg.WorkspaceDir) })
	mustRegister(reg, func() (tools.Tool, error) { return tools.NewWriteFile(cfg.WorkspaceDir) })
	mustRegister(reg, func() (tools.Tool, error) { return tools.NewListFiles(cfg.WorkspaceDir) })
	reg.Register(tools.NewExec(cfg.WorkspaceDir, cfg.ExecAllowList))

	engine := agent.New(cfg, provider, reg, st, agentReg)
	engine.SetProviderLookup(providerStore)
	srv := server.New(cfg, engine, agentReg, reg, st, providerStore, scaff)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("agenticgo starting")
	log.Printf("  llm:       %s @ %s (model %s)", provider.Name(), cfg.LLMBaseURL, cfg.LLMModel)
	log.Printf("  data:      %s", cfg.DataDir)
	log.Printf("  agents:    %s", cfg.AgentsDir)
	log.Printf("  tools:     %d registered", len(reg.Specs()))

	if err := srv.Start(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
	log.Println("agenticgo stopped")
}

func mustRegister(reg *tools.Registry, make func() (tools.Tool, error)) {
	t, err := make()
	if err != nil {
		log.Fatalf("tool init: %v", err)
	}
	reg.Register(t)
}
