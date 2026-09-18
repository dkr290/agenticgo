// Package agents manages agent definitions stored on the filesystem.
//
// Each agent is a directory under the agents root, named by its key:
//
//	agents/
//	  researcher/
//	    SOUL.md        # persona: tone, values, behavioral guidelines (required-ish)
//	    AGENTS.md      # operating instructions: how to approach tasks, use tools
//	    IDENTITY.md    # name, role, short description
//	    skills/        # SKILL.md files (see internal/skills)
//	    workspace/     # per-agent file jail for tools
//
// The idea mirrors GoClaw/OpenClaw context files, but stored as plain files and
// composed into the system prompt at run time. No database, no tenancy.
package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dkr290/agenticgo/internal/agenttemplates"
)

// ContextFile is a single markdown file that shapes an agent.
type ContextFile struct {
	Name    string `json:"name"`    // e.g. SOUL.md
	Content string `json:"content"` // raw markdown
	Exists  bool   `json:"exists"`  // false when the file is not on disk yet
}

// AgentConfig holds per-agent LLM settings. Fields are pointers so a nil
// value means "inherit" (use the provider/default), letting agents share the
// same defaults without duplicating them. Persisted as config.json in the
// agent directory.
type AgentConfig struct {
	Provider    *string  `json:"provider,omitempty"`    // named provider override (see internal/providers)
	Model       *string  `json:"model,omitempty"`       // model override (empty = provider default)
	Temperature *float64 `json:"temperature,omitempty"` // sampling temperature (0.0-2.0)
	MaxTokens   *int     `json:"max_tokens,omitempty"`  // max output tokens
	// Vision overrides the provider's vision capability for this agent — set it
	// when the agent pins a different Model than the provider's default and that
	// model does/doesn't support images. nil = inherit the provider's Vision flag.
	Vision *bool `json:"vision,omitempty"`
	// EnabledSkills lists keys from the global skills library that are active
	// for this agent. nil/empty = none: skills are never inherited implicitly,
	// they must be enabled per agent (UI Skills tab or this field).
	EnabledSkills []string `json:"enabled_skills,omitempty"`
	// EnabledTools lists namespaced MCP tool names
	// ("mcp_<server>_<tool>") this agent may use. nil/empty = none: MCP
	// tools are never inherited implicitly, they must be enabled per agent
	// (UI MCP Tools tab or this field). Built-in tools are gated by the
	// global AGENTICGO_TOOL_ALLOWLIST instead.
	EnabledTools []string `json:"enabled_tools,omitempty"`
	// EnabledCommands lists extra (dangerous) exec command names from
	// AGENTICGO_EXTRA_EXEC_COMMANDS that this agent may run via the exec
	// tool. nil/empty = none: extra commands are never inherited implicitly,
	// they must be enabled per agent (UI Extra Dangerous Exec Commands tab
	// or this field). Safe built-in commands are gated by the global
	// AGENTICGO_EXEC_ALLOWLIST instead.
	EnabledCommands []string `json:"enabled_commands,omitempty"`
}

// configFileName is where a per-agent LLM config is stored on disk.
const configFileName = "config.json"

// Agent is a loaded agent definition.
type Agent struct {
	Key         string        `json:"key"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Dir         string        `json:"-"`
	Files       []ContextFile `json:"files"`  // ordered context files present on disk
	Config      AgentConfig   `json:"config"` // per-agent LLM settings
}

// contextFileNames are the known context files, in the order they are composed
// into the system prompt. Files that don't exist on disk are still listed in
// the UI (as empty) so they can be created.
var contextFileNames = []string{
	"AGENTS.md",
	"SOUL.md",
	"IDENTITY.md",
	"USER.md",
	"USER_PREDEFINED.md",
	"CAPABILITIES.md",
	"HEARTBEAT.md",
}

// keyRE validates agent keys: lowercase alphanumeric, dash, underscore.
var keyRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ValidKey reports whether key is a safe agent key (used as a dir name).
func ValidKey(key string) bool {
	return key != "" && len(key) <= 64 && keyRE.MatchString(key)
}

// Registry loads and saves agents under a root directory.
type Registry struct {
	root string
}

// NewRegistry creates a registry rooted at dir, ensuring it exists.
func NewRegistry(root string) (*Registry, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create agents dir: %w", err)
	}
	return &Registry{root: root}, nil
}

// Root returns the agents root directory.
func (r *Registry) Root() string { return r.root }

// dirFor returns the directory for an agent key, validating the key.
func (r *Registry) dirFor(key string) (string, error) {
	if !ValidKey(key) {
		return "", fmt.Errorf("invalid agent key %q (use a-z, 0-9, -, _)", key)
	}
	return filepath.Join(r.root, key), nil
}

// WorkspaceDir returns (creating if needed) the per-agent workspace dir used
// to jail filesystem/exec tools.
func (r *Registry) WorkspaceDir(key string) (string, error) {
	dir, err := r.dirFor(key)
	if err != nil {
		return "", err
	}
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return "", fmt.Errorf("create agent workspace: %w", err)
	}
	return ws, nil
}

// SkillsLibraryDir returns (creating if needed) the global skills library:
// one shared directory where skills are uploaded once and from which agents
// enable them individually. Lives at <agentsRoot>/../skills (data/skills).
func (r *Registry) SkillsLibraryDir() (string, error) {
	lib := filepath.Join(filepath.Dir(r.root), "skills")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		return "", fmt.Errorf("create skills library dir: %w", err)
	}
	return lib, nil
}

// ImagesDir returns (creating if needed) the per-agent images dir, where
// reference images (screenshots, pictures) are stored for vision-capable runs.
func (r *Registry) ImagesDir(key string) (string, error) {
	dir, err := r.dirFor(key)
	if err != nil {
		return "", err
	}
	id := filepath.Join(dir, "images")
	if err := os.MkdirAll(id, 0o755); err != nil {
		return "", fmt.Errorf("create agent images dir: %w", err)
	}
	return id, nil
}

// SetEnabledSkills replaces the agent's enabled-skills list in its
// config.json and returns the refreshed agent.
func (r *Registry) SetEnabledSkills(key string, skillKeys []string) (*Agent, error) {
	dir, err := r.dirFor(key)
	if err != nil {
		return nil, err
	}
	if !r.Exists(key) {
		return nil, fmt.Errorf("agent %q not found", key)
	}
	cfg := r.loadConfig(dir)
	cfg.EnabledSkills = skillKeys
	if err := r.saveConfig(dir, cfg); err != nil {
		return nil, err
	}
	return r.Get(key)
}

// SetEnabledTools replaces the agent's enabled MCP-tools list in its
// config.json and returns the refreshed agent.
func (r *Registry) SetEnabledTools(key string, toolNames []string) (*Agent, error) {
	dir, err := r.dirFor(key)
	if err != nil {
		return nil, err
	}
	if !r.Exists(key) {
		return nil, fmt.Errorf("agent %q not found", key)
	}
	cfg := r.loadConfig(dir)
	cfg.EnabledTools = toolNames
	if err := r.saveConfig(dir, cfg); err != nil {
		return nil, err
	}
	return r.Get(key)
}

// SetEnabledCommands replaces the agent's enabled extra-exec-commands list in
// its config.json and returns the refreshed agent.
func (r *Registry) SetEnabledCommands(key string, cmds []string) (*Agent, error) {
	dir, err := r.dirFor(key)
	if err != nil {
		return nil, err
	}
	if !r.Exists(key) {
		return nil, fmt.Errorf("agent %q not found", key)
	}
	cfg := r.loadConfig(dir)
	cfg.EnabledCommands = cmds
	if err := r.saveConfig(dir, cfg); err != nil {
		return nil, err
	}
	return r.Get(key)
}

// --- Agent images (reference pictures/screenshots for vision models) ---

// Image is one stored reference image for an agent.
type Image struct {
	Name string `json:"name"` // base filename, e.g. "diagram.png"
	Size int64  `json:"size"` // bytes
}

// maxImageBytes caps a single uploaded image (kept small; it is base64'd into
// the LLM request, so large images blow up token/cost).
const maxImageBytes = 8 << 20 // 8 MB

// imageExt allow-lists image types we accept and can mime-type for the API.
var imageExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// ListImages returns the agent's stored images, sorted by name.
func (r *Registry) ListImages(key string) ([]Image, error) {
	dir, err := r.ImagesDir(key)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read images dir: %w", err)
	}
	out := []Image{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if _, ok := imageExt[ext]; !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Image{Name: e.Name(), Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SaveImage validates and writes an image into the agent's images dir. The
// filename is sanitized to a safe base name.
func (r *Registry) SaveImage(key, filename string, data []byte) (*Image, error) {
	if !r.Exists(key) {
		return nil, fmt.Errorf("agent %q not found", key)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty image")
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("image too large (max %d MB)", maxImageBytes>>20)
	}
	base := filepath.Base(strings.TrimSpace(filename))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return nil, fmt.Errorf("invalid filename")
	}
	ext := strings.ToLower(filepath.Ext(base))
	if _, ok := imageExt[ext]; !ok {
		return nil, fmt.Errorf("unsupported image type %q (use png, jpg, gif, webp)", ext)
	}
	dir, err := r.ImagesDir(key)
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, base)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return nil, fmt.Errorf("write image: %w", err)
	}
	return &Image{Name: base, Size: int64(len(data))}, nil
}

// DeleteImage removes an image from the agent's images dir.
func (r *Registry) DeleteImage(key, name string) error {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." {
		return fmt.Errorf("invalid image name")
	}
	if _, ok := imageExt[strings.ToLower(filepath.Ext(base))]; !ok {
		return fmt.Errorf("invalid image name %q", name)
	}
	dir, err := r.ImagesDir(key)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, base)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("image %q not found", name)
		}
		return fmt.Errorf("delete image: %w", err)
	}
	return nil
}

// ReadImage returns the raw bytes and MIME type of one of the agent's images.
func (r *Registry) ReadImage(key, name string) (data []byte, mime string, err error) {
	base := filepath.Base(strings.TrimSpace(name))
	mime, ok := imageExt[strings.ToLower(filepath.Ext(base))]
	if !ok || base == "" || base == "." {
		return nil, "", fmt.Errorf("invalid image name %q", name)
	}
	dir, err := r.ImagesDir(key)
	if err != nil {
		return nil, "", err
	}
	data, err = os.ReadFile(filepath.Join(dir, base))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("image %q not found", name)
		}
		return nil, "", fmt.Errorf("read image: %w", err)
	}
	return data, mime, nil
}

// List returns all agents present on disk, sorted by key.
func (r *Registry) List() ([]Agent, error) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return nil, fmt.Errorf("read agents dir: %w", err)
	}
	var out []Agent
	for _, e := range entries {
		if !e.IsDir() || !ValidKey(e.Name()) {
			continue
		}
		ag, err := r.Get(e.Name())
		if err != nil {
			continue
		}
		out = append(out, *ag)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Exists reports whether an agent exists.
func (r *Registry) Exists(key string) bool {
	dir, err := r.dirFor(key)
	if err != nil {
		return false
	}
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}

// Get loads a single agent and its context files.
func (r *Registry) Get(key string) (*Agent, error) {
	dir, err := r.dirFor(key)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("agent %q not found", key)
	}

	ag := &Agent{Key: key, Name: key, Dir: dir}
	ag.Config = r.loadConfig(dir)
	for _, name := range contextFileNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			// File optional: still list it (empty) so the UI can create it.
			ag.Files = append(ag.Files, ContextFile{Name: name})
			continue
		}
		content := string(data)
		ag.Files = append(ag.Files, ContextFile{Name: name, Content: content, Exists: true})
		if name == "IDENTITY.md" {
			ag.Name, ag.Description = parseIdentity(content, key)
		}
	}
	return ag, nil
}

// Create scaffolds a new agent with template context files and an initial
// LLM config.
func (r *Registry) Create(key, name, description, soul string, cfg AgentConfig) (*Agent, error) {
	dir, err := r.dirFor(key)
	if err != nil {
		return nil, err
	}
	if r.Exists(key) {
		return nil, fmt.Errorf("agent %q already exists", key)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}
	if err := r.saveConfig(dir, cfg); err != nil {
		return nil, err
	}

	if strings.TrimSpace(name) == "" {
		name = key
	}

	// Render the initial context files from the embedded templates. These are
	// the starting content only; afterwards the files live on disk and are
	// edited through the GUI. A non-empty soul argument overrides the template.
	tplData := agenttemplates.Data{Name: name, Role: strings.TrimSpace(description)}
	files := map[string]string{}
	for _, fname := range contextFileNames {
		content, err := agenttemplates.Render(fname, tplData)
		if err != nil {
			return nil, err
		}
		files[fname] = content
	}
	if strings.TrimSpace(soul) != "" {
		files["SOUL.md"] = soul
	}
	for fname, content := range files {
		if err := os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", fname, err)
		}
	}
	// Create the workspace subdir so tools have a jail.
	if _, err := r.WorkspaceDir(key); err != nil {
		return nil, err
	}
	return r.Get(key)
}

// Delete removes an agent directory entirely.
func (r *Registry) Delete(key string) error {
	dir, err := r.dirFor(key)
	if err != nil {
		return err
	}
	if !r.Exists(key) {
		return fmt.Errorf("agent %q not found", key)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	return nil
}

// ReadFile returns the content of a context file for an agent.
func (r *Registry) ReadFile(key, name string) (string, error) {
	if !validContextFile(name) {
		return "", fmt.Errorf("invalid context file %q", name)
	}
	dir, err := r.dirFor(key)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // treat missing as empty so the UI can create it
		}
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return string(data), nil
}

// WriteFile writes a context file for an agent.
func (r *Registry) WriteFile(key, name, content string) error {
	if !validContextFile(name) {
		return fmt.Errorf("invalid context file %q", name)
	}
	dir, err := r.dirFor(key)
	if err != nil {
		return err
	}
	if !r.Exists(key) {
		return fmt.Errorf("agent %q not found", key)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// validContextFile guards against path traversal on context file names.
func validContextFile(name string) bool {
	for _, n := range contextFileNames {
		if name == n {
			return true
		}
	}
	return false
}

// loadConfig reads the agent's config.json (if present). A missing or corrupt
// file yields the zero-value config, meaning "inherit all defaults".
func (r *Registry) loadConfig(dir string) AgentConfig {
	data, err := os.ReadFile(filepath.Join(dir, configFileName))
	if err != nil {
		return AgentConfig{}
	}
	var cfg AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return AgentConfig{}
	}
	return cfg
}

// saveConfig writes the agent's config.json into its directory.
func (r *Registry) saveConfig(dir string, cfg AgentConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFileName), data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// UpdateConfig replaces an agent's LLM config and returns the refreshed agent.
func (r *Registry) UpdateConfig(key string, cfg AgentConfig) (*Agent, error) {
	dir, err := r.dirFor(key)
	if err != nil {
		return nil, err
	}
	if !r.Exists(key) {
		return nil, fmt.Errorf("agent %q not found", key)
	}
	if err := r.saveConfig(dir, cfg); err != nil {
		return nil, err
	}
	return r.Get(key)
}

// SystemPrompt composes the system prompt for an agent from its context files.
func (a *Agent) SystemPrompt() string {
	var b strings.Builder
	for _, f := range a.Files {
		content := strings.TrimSpace(f.Content)
		if content == "" {
			continue
		}
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// parseIdentity extracts a display name and role from IDENTITY.md.
// It looks for "**Name:** X" and "**Role:** Y" lines; falls back to key.
func parseIdentity(content, key string) (name, desc string) {
	name, desc = key, ""
	for _, line := range strings.Split(content, "\n") {
		l := strings.TrimSpace(line)
		l = strings.Trim(l, "*") // strip bold markers at edges
		if strings.HasPrefix(l, "Name:") {
			if v := strings.TrimSpace(strings.TrimPrefix(l, "Name:")); v != "" {
				name = strings.Trim(v, "* ")
			}
		}
		if strings.HasPrefix(l, "Role:") {
			if v := strings.TrimSpace(strings.TrimPrefix(l, "Role:")); v != "" {
				desc = strings.Trim(v, "* ")
			}
		}
	}
	return name, desc
}
