package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
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

// resolve validates a relative path; os.Root enforces containment at use time,
// including symlinks and concurrent path changes.
func (w *workspace) resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("path escapes workspace: %q", p)
	}
	return filepath.Clean(p), nil
}

// --- read_file ---

type readFileArgs struct {
	Path string `json:"path" jsonschema:"Path to the file, relative to the workspace."`
}

// NewReadFile creates the read_file tool, jailed to root.
func NewReadFile(root string) (tool.FuncTool, error) {
	ws, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return functool.New(functool.Config{
		Name:        "read_file",
		Description: "Read the contents of a text file in the workspace. Do not use for images — use fetch_agent_image instead.",
	}, ws.readFile)
}

func (w *workspace) readFile(ctx context.Context, args readFileArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	full, err := w.resolve(args.Path)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(w.root)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	defer root.Close()
	f, err := root.Open(full)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	const max = 32 * 1024
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if len(data) > max {
		return string(data[:max]) + "\n... [truncated]", nil
	}
	return string(data), nil
}

// --- write_file ---

type writeFileArgs struct {
	Path    string `json:"path"    jsonschema:"Path to the file, relative to the workspace."`
	Content string `json:"content" jsonschema:"Content to write."`
}

// NewWriteFile creates the write_file tool, jailed to root.
func NewWriteFile(root string) (tool.FuncTool, error) {
	ws, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return functool.New(functool.Config{
		Name:        "write_file",
		Description: "Write content to a file in the workspace (overwrites).",
	}, ws.writeFile)
}

func (w *workspace) writeFile(ctx context.Context, args writeFileArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	full, err := w.resolve(args.Path)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(w.root)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}
	if err := root.WriteFile(full, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), nil
}

// --- list_files ---

// listFilesArgs takes an optional path; empty (or omitted) lists the
// workspace root.
type listFilesArgs struct {
	Path string `json:"path,omitempty" jsonschema:"Directory to list, relative to the workspace. Empty for root."`
}

// NewListFiles creates the list_files tool, jailed to root.
func NewListFiles(root string) (tool.FuncTool, error) {
	ws, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return functool.New(functool.Config{
		Name:        "list_files",
		Description: "List files and directories under a path in the workspace.",
	}, ws.listFiles)
}

func (w *workspace) listFiles(ctx context.Context, args listFilesArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p := args.Path
	if p == "" {
		p = "."
	}
	full, err := w.resolve(p)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(w.root)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	defer root.Close()
	f, err := root.Open(full)
	if err != nil {
		return "", fmt.Errorf("open directory: %w", err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
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
