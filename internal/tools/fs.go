package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspace jails filesystem operations to a root directory.
type workspace struct {
	root string
}

func newWorkspace(root string) (*workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	return &workspace{root: abs}, nil
}

// resolve maps a model-supplied path into the jail, rejecting escapes.
func (w *workspace) resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	// Clean and join inside the root.
	clean := filepath.Clean("/" + p) // force relative interpretation
	full := filepath.Join(w.root, clean)

	// Ensure the result stays inside root.
	rootWithSep := w.root + string(os.PathSeparator)
	if full != w.root && !strings.HasPrefix(full, rootWithSep) {
		return "", fmt.Errorf("path escapes workspace: %q", p)
	}
	return full, nil
}

// --- read_file ---

type readFileTool struct{ ws *workspace }

// NewReadFile creates the read_file tool.
func NewReadFile(root string) (Tool, error) {
	ws, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return &readFileTool{ws: ws}, nil
}

func (t *readFileTool) Name() string        { return "read_file" }
func (t *readFileTool) Description() string { return "Read the contents of a file in the workspace." }
func (t *readFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Path to the file, relative to the workspace."},
		},
		"required": []string{"path"},
	}
}

func (t *readFileTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	full, err := t.ws.resolve(in.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	const max = 32 * 1024
	if len(data) > max {
		return string(data[:max]) + "\n... [truncated]", nil
	}
	return string(data), nil
}

// --- write_file ---

type writeFileTool struct{ ws *workspace }

// NewWriteFile creates the write_file tool.
func NewWriteFile(root string) (Tool, error) {
	ws, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return &writeFileTool{ws: ws}, nil
}

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Write content to a file in the workspace (overwrites)."
}
func (t *writeFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path to the file, relative to the workspace."},
			"content": map[string]any{"type": "string", "description": "Content to write."},
		},
		"required": []string{"path", "content"},
	}
}

func (t *writeFileTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	full, err := t.ws.resolve(in.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}
	if err := os.WriteFile(full, []byte(in.Content), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
}

// --- list_files ---

type listFilesTool struct{ ws *workspace }

// NewListFiles creates the list_files tool.
func NewListFiles(root string) (Tool, error) {
	ws, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return &listFilesTool{ws: ws}, nil
}

func (t *listFilesTool) Name() string { return "list_files" }
func (t *listFilesTool) Description() string {
	return "List files and directories under a path in the workspace."
}
func (t *listFilesTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory to list, relative to the workspace. Empty for root."},
		},
	}
}

func (t *listFilesTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &in) // path optional

	full, err := t.ws.resolve(in.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return "", fmt.Errorf("list dir: %w", err)
	}
	var b strings.Builder
	for _, e := range entries {
		kind := "file"
		if e.IsDir() {
			kind = "dir"
		}
		fmt.Fprintf(&b, "%s\t%s\n", kind, e.Name())
	}
	if b.Len() == 0 {
		return "(empty)", nil
	}
	return b.String(), nil
}
