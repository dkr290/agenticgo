package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// execTool sandboxes standard commands; explicitly enabled dangerous extras
// run with the agenticgo process's container/host privileges.
type execTool struct {
	workspace string
	allow     map[string]bool
	timeout   time.Duration
	// extra lists the dangerous-but-enabled commands beyond the base
	// allow-list, so the model can see them in the tool description.
	extra []string
}

type execArgs struct {
	Command string `json:"command" jsonschema:"The command line to run, e.g. \"ls -la\". Only allow-listed commands are permitted."`
}

// NewExec creates the exec tool. allowList contains permitted command names
// (e.g. "ls", "cat"). workspace is the working directory for commands.
func NewExec(workspace string, allowList []string) tool.FuncTool {
	return NewExecWithExtra(workspace, allowList, nil)
}

// NewExecWithExtra creates the exec tool like NewExec but records which
// commands came from the extra (dangerous) allow-list so the description can
// name them — otherwise the model cannot tell which extra commands it may run.
func NewExecWithExtra(workspace string, allowList, extra []string) tool.FuncTool {
	allow := map[string]bool{}
	for _, c := range allowList {
		allow[c] = true
	}
	t := &execTool{workspace: workspace, allow: allow, timeout: 30 * time.Second, extra: extra}
	return functool.MustNew(functool.Config{
		Name:        "exec",
		Description: t.description(),
	}, t.run)
}

// description names the enabled extra commands so the model can tell which
// dangerous commands it may run beyond the safe standard set.
func (t *execTool) description() string {
	d := "Run an allow-listed command (arguments are whitespace-separated). Standard commands run in an isolated workspace sandbox."
	if len(t.extra) > 0 {
		d += " In addition to the standard safe commands, you may run these " +
			"extra commands with full process/container privileges: " +
			strings.Join(t.extra, ", ") + "."
	}
	return d
}

// run is the tool body. functool binds and validates the arguments (required
// "command", no additional properties) before this is called.
func (t *execTool) run(ctx context.Context, args execArgs) (string, error) {
	fields := strings.Fields(args.Command)
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

	var cmd *exec.Cmd
	if slices.Contains(t.extra, cmdName) {
		cmd = exec.CommandContext(ctx, cmdName, fields[1:]...)
		cmd.Dir = t.workspace
	} else {
		var err error
		cmd, err = sandboxCommand(ctx, t.workspace, fields)
		if err != nil {
			return "", err
		}
	}
	cmd.WaitDelay = time.Second

	stdout, stderr := cappedOutput{limit: 16 * 1024}, cappedOutput{limit: 16 * 1024}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("exec %q: %w\nstderr: %s", cmdName, err, strings.TrimSpace(stderr.String()))
	}

	out := stdout.String()
	if stdout.truncated {
		out += "\n... [truncated]"
	}
	if out == "" {
		out = "(no output)"
	}
	return out, nil
}

// sandboxCommand exposes only read-only system executables/libraries and the
// agent workspace. No application data, credentials, host network, or inherited
// environment is available. Failure to establish the sandbox is an error.
func sandboxCommand(ctx context.Context, workspace string, fields []string) (*exec.Cmd, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("standard exec sandbox requires Linux and bubblewrap")
	}
	if fields[0] == "find" {
		for _, arg := range fields[1:] {
			if slices.Contains([]string{"-exec", "-execdir", "-ok", "-okdir"}, arg) {
				return nil, fmt.Errorf("find subprocess actions are not allowed")
			}
		}
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("standard exec requires bubblewrap (bwrap): %w", err)
	}
	program, err := exec.LookPath(fields[0])
	if err != nil {
		return nil, fmt.Errorf("find command: %w", err)
	}
	program, err = filepath.EvalSymlinks(program)
	if err != nil {
		return nil, fmt.Errorf("resolve command: %w", err)
	}
	if !strings.HasPrefix(program, "/usr/") && !strings.HasPrefix(program, "/bin/") {
		return nil, fmt.Errorf("standard command must be installed under /usr or /bin: %s", program)
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	args := []string{"--die-with-parent", "--new-session", "--unshare-all", "--cap-drop", "ALL", "--clearenv"}
	for _, dir := range []string{"/usr", "/bin", "/lib", "/lib64"} {
		if _, err := os.Stat(dir); err == nil {
			args = append(args, "--ro-bind", dir, dir)
		}
	}
	args = append(args, "--tmpfs", "/tmp", "--dev", "/dev", "--bind", root, "/workspace", "--chdir", "/workspace",
		"--setenv", "PATH", "/usr/bin:/bin", "--setenv", "HOME", "/workspace", "--setenv", "LANG", "C.UTF-8", "--", program)
	args = append(args, fields[1:]...)
	return exec.CommandContext(ctx, bwrap, args...), nil
}

type cappedOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedOutput) String() string { return b.buffer.String() }

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, b.limit-b.buffer.Len())
	_, _ = b.buffer.Write(p[:keep])
	if keep < n {
		b.truncated = true
	}
	return n, nil
}
