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
	"github.com/dkr290/agenticgo/internal/cron"
	"github.com/dkr290/agenticgo/internal/crypto"
	"github.com/dkr290/agenticgo/internal/logger"
	"github.com/dkr290/agenticgo/internal/mcp"
	"github.com/dkr290/agenticgo/internal/providers"
	"github.com/dkr290/agenticgo/internal/server"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/dkr290/agenticgo/internal/tools"
	"github.com/microsoft/agent-framework-go/tool"
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

	// Agent registry (one directory per agent under AgentsDir). No agent is
	// seeded: a provider must be configured first anyway, so agents are
	// created manually from the UI.
	agentReg, err := agents.NewRegistry(cfg.AgentsDir)
	if err != nil {
		log.Fatalf("agents: %v", err)
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

	// Encryption key for provider API keys at rest (AGENTICGO_SECRET_KEY from
	// config, else a generated data/secret.key file).
	encKey, err := crypto.LoadKey(cfg.SecretKey, filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		log.Fatalf("secret key: %v", err)
	}

	// Named provider store (editable from the Providers UI). This is the single
	// source of LLM providers for chat: the store's "default" provider backs the
	// engine unless a request or an agent's config names a specific provider.
	// Nothing is seeded — providers are created manually from the UI. API keys
	// are encrypted at rest.
	providerStore, err := providers.Open(filepath.Join(cfg.DataDir, "providers.json"), encKey)
	if err != nil {
		log.Fatalf("providers: %v", err)
	}
	providerStore.SetLogger(lg)

	// MCP server manager (data/mcp_servers.json). Connections are manual:
	// use the Connect button on the MCP Servers page to spawn/dial a server
	// and discover its tools, then enable them per agent (MCP Tools tab).
	mcpMgr, err := mcp.Open(filepath.Join(cfg.DataDir, "mcp_servers.json"))
	if err != nil {
		log.Fatalf("mcp servers: %v", err)
	}
	mcpMgr.SetLogger(lg)
	defer mcpMgr.CloseAll()

	// Tool registry with built-ins, gated by the tool allow-list.
	reg := tools.NewRegistry(cfg.ToolAllowList)
	mustRegister(reg, func() (tools.Tool, error) { return adaptTool(tools.NewReadFile(cfg.WorkspaceDir)) })
	mustRegister(reg, func() (tools.Tool, error) { return adaptTool(tools.NewWriteFile(cfg.WorkspaceDir)) })
	mustRegister(reg, func() (tools.Tool, error) { return adaptTool(tools.NewListFiles(cfg.WorkspaceDir)) })
	reg.Register(tools.AdaptFuncTool(tools.NewExec(cfg.WorkspaceDir, cfg.ExecAllowList)))

	engine := agent.New(cfg, reg, st, agentReg)
	engine.SetProviderLookup(providerStore)
	engine.SetMCPManager(mcpMgr)

	// Cron scheduler (data/cron.json). Jobs run an agent on a schedule; each
	// enabled job executes Engine.Run on its own cron-<id> session.
	cronSched, err := cron.Open(filepath.Join(cfg.DataDir, "cron.json"), func(ctx context.Context, agentKey, session, message string) error {
		_, err := engine.Run(ctx, agentKey, session, message, "", nil, nil)
		return err
	})
	if err != nil {
		log.Fatalf("cron: %v", err)
	}
	cronSched.SetLogger(lg)

	srv := server.New(cfg, engine, agentReg, reg, st, providerStore, cronSched, mcpMgr)
	srv.SetLogger(lg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cronSched.Start()
	defer cronSched.Stop()

	// Background retention sweep: periodically expire stale knowledge and
	// observations for ALL agents, including idle ones (the per-Run prune is
	// lazy and only fires when an agent runs). Starts only when retention is
	// configured (some TTL/Keep knob non-zero); stops with the shared ctx.
	engine.StartRetentionSweeper(ctx)

	log.Printf("agenticgo starting")
	if d := providerStore.Default(); d != nil {
		log.Printf("  llm:       %s @ %s (model %s)", d.Name, d.BaseURL, d.Model)
	}
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

// adaptTool adapts a functool constructor result to the legacy registry.
func adaptTool(ft tool.FuncTool, err error) (tools.Tool, error) {
	if err != nil {
		return nil, err
	}
	return tools.AdaptFuncTool(ft), nil
}
