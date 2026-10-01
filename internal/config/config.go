// Package config loads runtime configuration from environment variables.
// Configuration is 12-factor style so the binary is k8s-friendly.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for agenticgo.
type Config struct {
	// HTTP listen address, e.g. ":8080".
	Addr string

	// DataDir is where SQLite and the workspace live.
	DataDir string

	// AgentsDir is the root containing one directory per agent (context files,
	// skills, workspace). Defaults to DataDir/agents.
	AgentsDir string

	// WorkspaceDir is the fallback jail root for filesystem tools when an agent
	// has no workspace. Per-agent workspaces live under AgentsDir/<key>/workspace.
	WorkspaceDir string

	// ExecAllowList is the set of command names the exec tool may run.
	ExecAllowList []string

	// ExtraExecCommands are additional, potentially dangerous command names
	// (e.g. kubectl, git) baked into the deployment image and declared via
	// AGENTICGO_EXTRA_EXEC_COMMANDS. They are NOT on the exec allow-list by
	// default: each agent must explicitly enable them (config.json
	// enabled_commands, Agents → Extra Dangerous Exec Commands tab) before the
	// exec tool will run them.
	ExtraExecCommands []string

	// ToolAllowList restricts which tools are exposed. Defaults to all built-ins;
	// remove entries to disable tools.
	ToolAllowList []string

	// MaxAgentIterations bounds the tool-use loop to avoid runaway agents.
	MaxAgentIterations int

	// Observation retention: observations older than ObservationTTLDays are
	// pruned, and each agent keeps at most ObservationKeepLatest. This is the
	// anti-hallucination control for recurring monitoring agents.
	ObservationTTLDays    int
	ObservationKeepLatest int

	// ObservationInject is the app-wide default for how many recent
	// observations are injected into an agent's system prompt (1 = only the
	// latest, the historical behavior). An agent may override it via
	// config.json observation_inject (nil = inherit this default).
	ObservationInject int

	// KnowledgeInject is how many recent curated-knowledge entries are
	// injected into an agent's system prompt (app-wide; not per-agent).
	KnowledgeInject int

	// Knowledge retention optionally bounds how much self-educated knowledge an
	// agent accumulates (separate from the GUI-uploaded knowledge_docs, which
	// are never auto-pruned). KnowledgeTTLDays prunes entries older than that
	// many days; KnowledgeKeepLatest caps how many of the newest entries are
	// kept per agent. Both default to 0 = disabled (keep forever); see Load for
	// examples. This is opt-in retention for long-lived self-educating agents.
	KnowledgeTTLDays    int
	KnowledgeKeepLatest int

	// SecretKey is the base64-encoded 32-byte master key used to encrypt
	// provider API keys at rest (AGENTICGO_SECRET_KEY). No default on purpose:
	// when empty, a random key is generated once and persisted to
	// DataDir/secret.key (0600) — supplying it via env (e.g. from a secrets
	// manager) is stronger because the key never touches disk.
	SecretKey string

	// Debug enables verbose debug logging to stderr (AGENTICGO_DEBUG=true).
	Debug bool

	// SystemPrompt is the base system prompt; knowledge is appended to it.
	SystemPrompt string
}

// Load reads configuration from environment variables, applying defaults.
func Load() (*Config, error) {
	dataDir := getEnv("AGENTICGO_DATA_DIR", "data")

	cfg := &Config{
		Addr:    getEnv("AGENTICGO_ADDR", ":8080"),
		DataDir: dataDir,
		SystemPrompt: getEnv("AGENTICGO_SYSTEM_PROMPT",
			"You are agenticgo, a helpful AI agent. You can use tools to read and write files, "+
				"list directories, and run allow-listed commands inside a jailed workspace. "+
				"Think step by step and use tools when they help accomplish the user's task."),
	}

	cfg.AgentsDir = getEnv("AGENTICGO_AGENTS_DIR", dataDir+"/agents")
	cfg.WorkspaceDir = getEnv("AGENTICGO_WORKSPACE_DIR", dataDir+"/workspace")

	cfg.ExecAllowList = splitList(getEnv("AGENTICGO_EXEC_ALLOWLIST",
		"ls,cat,grep,find,echo,pwd,head,tail,wc,mkdir,touch,cp,mv,date"))
	cfg.ToolAllowList = splitList(getEnv("AGENTICGO_TOOL_ALLOWLIST",
		"read_file,write_file,list_files,exec"))
	cfg.ExtraExecCommands = sanitizeCommands(splitList(getEnv("AGENTICGO_EXTRA_EXEC_COMMANDS", "")))

	cfg.MaxAgentIterations = getEnvInt("AGENTICGO_MAX_ITERATIONS", 12)
	cfg.ObservationTTLDays = getEnvInt("AGENTICGO_OBSERVATION_TTL_DAYS", 14)
	cfg.ObservationKeepLatest = getEnvInt("AGENTICGO_OBSERVATION_KEEP", 200)
	cfg.ObservationInject = getEnvInt("AGENTICGO_OBSERVATION_INJECT", 1)
	cfg.KnowledgeInject = getEnvInt("AGENTICGO_KNOWLEDGE_INJECT", 25)
	// Retention for self-educated knowledge — OPT-IN (both default 0 = disabled,
	// i.e. keep everything forever, the historical behavior). Set them to bound
	// how much a long-lived agent can accumulate. Examples:
	//   AGENTICGO_KNOWLEDGE_TTL_DAYS=365  -> delete entries older than ~1 year
	//                                        (time-driven expiry; use 0 if you
	//                                        don't want time-based deletion)
	//   AGENTICGO_KNOWLEDGE_KEEP=1000     -> keep only the newest 1000 entries
	//                                        per agent (size cap)
	// Either can be set alone; both together apply age-then-count. Never touches
	// the GUI-uploaded knowledge-base documents (knowledge_docs), only the
	// agent's self-educated knowledge.
	cfg.KnowledgeTTLDays = getEnvInt("AGENTICGO_KNOWLEDGE_TTL_DAYS", 0)
	cfg.KnowledgeKeepLatest = getEnvInt("AGENTICGO_KNOWLEDGE_KEEP", 0)
	cfg.Debug = getEnvBool("AGENTICGO_DEBUG", false)

	// No default: empty means "generate/persist a key under DataDir".
	cfg.SecretKey = getEnv("AGENTICGO_SECRET_KEY", "")

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

func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
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

// sanitizeCommands drops any command names containing a path separator —
// extra exec commands must be bare names resolved via PATH, never paths.
func sanitizeCommands(cmds []string) []string {
	out := cmds[:0]
	for _, c := range cmds {
		if strings.ContainsAny(c, "/\\") {
			continue
		}
		out = append(out, c)
	}
	return out
}
