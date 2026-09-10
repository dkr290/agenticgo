// Package config loads runtime configuration from environment variables.
// Configuration is 12-factor style so the binary is k8s-friendly.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for agenticgo.
type Config struct {
	// HTTP listen address, e.g. ":8080".
	Addr string

	// OpenAI-compatible LLM settings.
	LLMBaseURL string // e.g. http://localhost:11434/v1 (Ollama) or LM Studio / vLLM
	LLMAPIKey  string // often unused for local providers
	LLMModel   string // e.g. qwen2.5, gpt-4o-mini, etc.

	// DataDir is where SQLite, the workspace, and the knowledge file live.
	DataDir string

	// AgentsDir is the root containing one directory per agent (context files,
	// skills, workspace). Defaults to DataDir/agents.
	AgentsDir string

	// WorkspaceDir is the fallback jail root for filesystem tools when an agent
	// has no workspace. Per-agent workspaces live under AgentsDir/<key>/workspace.
	WorkspaceDir string

	// KnowledgeFile is appended to during self-evolution and injected into the system prompt.
	// Defaults to DataDir/knowledge/KNOWLEDGE.md.
	KnowledgeFile string

	// ExecAllowList is the set of command names the exec tool may run.
	ExecAllowList []string

	// ToolAllowList restricts which tools are exposed. Empty means all built-ins.
	ToolAllowList []string

	// MaxAgentIterations bounds the tool-use loop to avoid runaway agents.
	MaxAgentIterations int

	// SystemPrompt is the base system prompt; knowledge is appended to it.
	SystemPrompt string
}

// Load reads configuration from environment variables, applying defaults.
func Load() (*Config, error) {
	dataDir := getEnv("AGENTICGO_DATA_DIR", "data")

	cfg := &Config{
		Addr:     getEnv("AGENTICGO_ADDR", ":8080"),
		DataDir:  dataDir,
		LLMModel: getEnv("AGENTICGO_LLM_MODEL", "qwen2.5"),
		SystemPrompt: getEnv("AGENTICGO_SYSTEM_PROMPT",
			"You are agenticgo, a helpful AI agent. You can use tools to read and write files, "+
				"list directories, and run allow-listed commands inside a jailed workspace. "+
				"Think step by step and use tools when they help accomplish the user's task."),
	}

	// Default to Ollama's OpenAI-compatible endpoint.
	cfg.LLMBaseURL = getEnv("AGENTICGO_LLM_BASE_URL", "http://localhost:11434/v1")
	cfg.LLMAPIKey = getEnv("AGENTICGO_LLM_API_KEY", "ollama")

	cfg.AgentsDir = getEnv("AGENTICGO_AGENTS_DIR", dataDir+"/agents")
	cfg.WorkspaceDir = getEnv("AGENTICGO_WORKSPACE_DIR", dataDir+"/workspace")
	cfg.KnowledgeFile = getEnv("AGENTICGO_KNOWLEDGE_FILE", dataDir+"/knowledge/KNOWLEDGE.md")

	cfg.ExecAllowList = splitList(getEnv("AGENTICGO_EXEC_ALLOWLIST",
		"ls,cat,grep,find,echo,pwd,head,tail,wc,mkdir,touch,cp,mv,date"))
	cfg.ToolAllowList = splitList(getEnv("AGENTICGO_TOOL_ALLOWLIST", ""))

	cfg.MaxAgentIterations = getEnvInt("AGENTICGO_MAX_ITERATIONS", 12)

	if cfg.LLMBaseURL == "" {
		return nil, fmt.Errorf("AGENTICGO_LLM_BASE_URL must not be empty")
	}
	if cfg.LLMModel == "" {
		return nil, fmt.Errorf("AGENTICGO_LLM_MODEL must not be empty")
	}

	return cfg, nil
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
