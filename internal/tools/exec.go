package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// execTool runs allow-listed commands jailed to the workspace directory.
type execTool struct {
	workspace string
	allow     map[string]bool
	timeout   time.Duration
}

// NewExec creates the exec tool. allowList contains permitted command names
// (e.g. "ls", "cat"). workspace is the working directory for commands.
func NewExec(workspace string, allowList []string) Tool {
	allow := map[string]bool{}
	for _, c := range allowList {
		allow[c] = true
	}
	return &execTool{workspace: workspace, allow: allow, timeout: 30 * time.Second}
}

func (t *execTool) Name() string { return "exec" }
func (t *execTool) Description() string {
	return "Run an allow-listed shell command inside the workspace."
}
func (t *execTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The command line to run, e.g. \"ls -la\". Only allow-listed commands are permitted.",
			},
		},
		"required": []string{"command"},
	}
}

func (t *execTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	fields := strings.Fields(in.Command)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmdName := fields[0]

	// Base-name match so "/bin/ls" still checks "ls", but we reject any path form
	// to keep the allow-list tight and avoid surprises.
	if strings.Contains(cmdName, "/") || strings.Contains(cmdName, "\\") {
		return "", fmt.Errorf("command must be a bare name, not a path: %q", cmdName)
	}
	if !t.allow[cmdName] {
		return "", fmt.Errorf("command %q is not on the exec allow-list", cmdName)
	}

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cmdName)
	if len(fields) > 1 {
		cmd.Args = append([]string{cmdName}, fields[1:]...)
	}
	cmd.Dir = t.workspace

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("exec %q: %w\nstderr: %s", cmdName, err, strings.TrimSpace(stderr.String()))
	}

	out := stdout.String()
	const max = 16 * 1024
	if len(out) > max {
		out = out[:max] + "\n... [truncated]"
	}
	if out == "" {
		out = "(no output)"
	}
	return out, nil
}
