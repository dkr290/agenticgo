package tools

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/tool"
)

func TestFilesystemContainment(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	read, err := NewReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	write, err := NewWriteFile(root)
	if err != nil {
		t.Fatal(err)
	}
	list, err := NewListFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		tool tool.FuncTool
		args string
	}{
		{"read symlink", read, `{"path":"escape/secret"}`},
		{"write symlink", write, `{"path":"escape/secret","content":"overwritten"}`},
		{"list symlink", list, `{"path":"escape"}`},
		{"parent", read, `{"path":"../secret"}`},
		{"absolute", read, `{"path":"/etc/passwd"}`},
		{"malformed list", list, `{"path":17}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.tool.Call(context.Background(), tc.args); err == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != "outside marker" {
		t.Fatalf("outside file changed: %q %v", data, err)
	}
	if _, err := write.Call(context.Background(), `{"path":"sub/file","content":"local"}`); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{}`, `{"path":""}`, `{"path":"."}`} {
		out, err := list.Call(context.Background(), args)
		outStr, _ := out.(string)
		if err != nil || !strings.Contains(outStr, "sub") {
			t.Fatalf("list root: %q %v", out, err)
		}
	}
}

func TestExecRejectsFindSubprocesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux sandbox")
	}
	tool := NewExec(t.TempDir(), []string{"find"})
	for _, action := range []string{"-exec", "-execdir", "-ok", "-okdir"} {
		args, _ := json.Marshal(map[string]string{"command": "find . " + action + " id ;"})
		if _, err := tool.Call(context.Background(), string(args)); err == nil || !strings.Contains(err.Error(), "subprocess") {
			t.Fatalf("%s: %v", action, err)
		}
	}
}

func TestExecSandboxAndExtraCommands(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux sandbox")
	}
	root, private := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "local"), []byte("workspace marker"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(private, "secret")
	if err := os.WriteFile(secret, []byte("private marker"), 0600); err != nil {
		t.Fatal(err)
	}
	probe, err := sandboxCommand(context.Background(), root, []string{"true"})
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("user namespaces unavailable: %v: %s", err, out)
	}
	tool := NewExec(root, []string{"cat", "pwd"})
	for _, tc := range []struct {
		cmd, want string
		fail      bool
	}{
		{"cat local", "workspace marker", false},
		{"pwd", "/workspace", false},
		{"cat " + secret, "", true},
		{"cat ../../" + strings.TrimPrefix(secret, "/"), "", true},
	} {
		args, _ := json.Marshal(map[string]string{"command": tc.cmd})
		out, err := tool.Call(context.Background(), string(args))
		outStr, _ := out.(string)
		if (err != nil) != tc.fail || (!tc.fail && !strings.Contains(outStr, tc.want)) || strings.Contains(outStr, "private marker") {
			t.Fatalf("%s: %q %v", tc.cmd, out, err)
		}
	}
}

func TestEnabledExtraUsesImagePathAndEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin, workspace := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "image-extra"), []byte("#!/bin/sh\nprintf '%s\\n' \"$AGENTICGO_TEST_CREDENTIAL\"\npwd\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("AGENTICGO_TEST_CREDENTIAL", "mounted-credential-marker")
	extra := NewExecWithExtra(workspace, []string{"image-extra"}, []string{"image-extra"})
	out, err := extra.Call(context.Background(), `{"command":"image-extra"}`)
	outStr, _ := out.(string)
	if err != nil || !strings.Contains(outStr, "mounted-credential-marker") || !strings.Contains(outStr, workspace) {
		t.Fatalf("extra command: %q %v", out, err)
	}
	standard := NewExec(workspace, []string{"image-extra"})
	if _, err := standard.Call(context.Background(), `{"command":"image-extra"}`); err == nil {
		t.Fatal("standard command ran without sandbox")
	}
	if _, err := exec.LookPath("bwrap"); err == nil {
		t.Fatal("test PATH unexpectedly contains bwrap")
	}
}

func TestOutputCaptureIsBoundedDuringCopy(t *testing.T) {
	output := cappedOutput{limit: 1024}
	// Hide WriterTo to exercise io.Copy's ReaderFrom detection, as pipe copying
	// can do. Embedding bytes.Buffer would accidentally bypass the limit.
	source := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 64*1024))}
	n, err := io.Copy(&output, source)
	if err != nil || n != 64*1024 || len(output.String()) != 1024 || !output.truncated {
		t.Fatalf("capture: read=%d retained=%d truncated=%v err=%v", n, len(output.String()), output.truncated, err)
	}
}
