package agents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// newReportsRegistry returns a registry with a "demo" agent and the path of
// its (created) reports directory.
func newReportsRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create("demo", "Demo", "test agent", "", AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reg.Root(), "demo", "workspace", ReportsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return reg, dir
}

// writeReport writes a file with a fixed modification time.
func writeReport(t *testing.T, dir, name, content string, mod time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func reportNames(list []Report) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.Name)
	}
	return out
}

func TestValidReportName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"security-audit-2026-11-19-1002.md", true},
		{"report with spaces.md", true},
		{"notes", true},
		{"", false},
		{".", false},
		{"..", false},
		{".hidden.md", false},
		{"a/b.md", false},
		{"../secret.md", false},
		{`a\b.md`, false},
		{"bad\x00name.md", false},
		{"tab\tname.md", false},
		{strings.Repeat("a", 256), false},
	}
	for _, tt := range tests {
		if got := ValidReportName(tt.name); got != tt.want {
			t.Errorf("ValidReportName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestListReportsNewestFirstRegularFilesOnly(t *testing.T) {
	reg, dir := newReportsRegistry(t)
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	writeReport(t, dir, "old.md", "old", base)
	writeReport(t, dir, "new.md", "new", base.Add(2*time.Hour))
	writeReport(t, dir, "mid.md", "mid", base.Add(time.Hour))
	writeReport(t, dir, ".partial.md", "hidden", base.Add(3*time.Hour))
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}

	list, err := reg.ListReports("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reportNames(list), []string{"new.md", "mid.md", "old.md"}; !slices.Equal(got, want) {
		t.Fatalf("ListReports = %v, want %v", got, want)
	}
	if list[0].Size != 3 || !list[0].Modified.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("metadata = %+v", list[0])
	}
}

func TestListReportsMissingDirAndUnknownAgent(t *testing.T) {
	reg, dir := newReportsRegistry(t)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	list, err := reg.ListReports("demo")
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("missing reports dir: list=%v err=%v, want empty list", list, err)
	}
	if _, err := reg.ListReports("ghost"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unknown agent: err=%v, want ErrAgentNotFound", err)
	}
}

func TestReadReport(t *testing.T) {
	reg, dir := newReportsRegistry(t)
	mod := time.Date(2026, 11, 19, 10, 2, 0, 0, time.UTC)
	writeReport(t, dir, "audit.md", "# Audit\n", mod)
	if err := os.Mkdir(filepath.Join(dir, "folder.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("audit.md", filepath.Join(dir, "inside-link.md")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "outside-link.md")); err != nil {
		t.Fatal(err)
	}

	data, info, err := reg.ReadReport("demo", "audit.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# Audit\n" || info.Name != "audit.md" || info.Size != 8 || !info.Modified.Equal(mod) {
		t.Fatalf("ReadReport = %q %+v", data, info)
	}

	tests := []struct {
		agent, name string
		want        error
	}{
		{"demo", "missing.md", ErrReportNotFound},
		{"demo", "folder.md", ErrReportNotFound},
		{"demo", "inside-link.md", ErrReportNotFound},
		{"demo", "outside-link.md", ErrReportNotFound},
		{"demo", "../config.json", ErrInvalidReportName},
		{"demo", "..", ErrInvalidReportName},
		{"demo", "", ErrInvalidReportName},
		{"ghost", "audit.md", ErrAgentNotFound},
	}
	for _, tt := range tests {
		if _, _, err := reg.ReadReport(tt.agent, tt.name); !errors.Is(err, tt.want) {
			t.Errorf("ReadReport(%q, %q) err = %v, want %v", tt.agent, tt.name, err, tt.want)
		}
	}
}

func TestReportsDirSymlinkCannotEscapeWorkspace(t *testing.T) {
	reg, dir := newReportsRegistry(t)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeReport(t, outside, "secret-1.md", "secret", time.Now().Add(-time.Hour))
	writeReport(t, outside, "secret-2.md", "secret", time.Now())
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}

	if data, _, err := reg.ReadReport("demo", "secret-2.md"); err == nil {
		t.Fatalf("read through a symlinked reports dir: %q", data)
	}
	if list, err := reg.ListReports("demo"); err == nil && len(list) > 0 {
		t.Fatalf("listed through a symlinked reports dir: %v", list)
	}
	_, _ = reg.PruneReports("demo", 1)
	for _, name := range []string{"secret-1.md", "secret-2.md"} {
		if _, err := os.Stat(filepath.Join(outside, name)); err != nil {
			t.Fatalf("prune followed the symlink and removed %s: %v", name, err)
		}
	}
}

func TestPruneReports(t *testing.T) {
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		keep        int
		wantRemoved int
		wantLeft    []string
	}{
		{"zero keeps everything", 0, 0, []string{"r4.md", "r3.md", "r2.md", "r1.md"}},
		{"negative keeps everything", -1, 0, []string{"r4.md", "r3.md", "r2.md", "r1.md"}},
		{"keeps the newest two", 2, 2, []string{"r4.md", "r3.md"}},
		{"keep above count removes nothing", 10, 0, []string{"r4.md", "r3.md", "r2.md", "r1.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, dir := newReportsRegistry(t)
			for i := 1; i <= 4; i++ {
				writeReport(t, dir, fmt.Sprintf("r%d.md", i), "x", base.Add(time.Duration(i)*time.Hour))
			}
			writeReport(t, dir, ".hidden", "x", base) // oldest, but never a report
			if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}

			removed, err := reg.PruneReports("demo", tt.keep)
			if err != nil {
				t.Fatal(err)
			}
			if removed != tt.wantRemoved {
				t.Fatalf("removed = %d, want %d", removed, tt.wantRemoved)
			}
			list, err := reg.ListReports("demo")
			if err != nil {
				t.Fatal(err)
			}
			if got := reportNames(list); !slices.Equal(got, tt.wantLeft) {
				t.Fatalf("left = %v, want %v", got, tt.wantLeft)
			}
			for _, untouched := range []string{".hidden", "sub"} {
				if _, err := os.Lstat(filepath.Join(dir, untouched)); err != nil {
					t.Fatalf("%s must be left alone: %v", untouched, err)
				}
			}
		})
	}
}

func TestPruneReportsWithoutReportsDir(t *testing.T) {
	reg, dir := newReportsRegistry(t)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if removed, err := reg.PruneReports("demo", 5); err != nil || removed != 0 {
		t.Fatalf("PruneReports = %d, %v; want 0, nil", removed, err)
	}
	if _, err := reg.PruneReports("ghost", 5); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unknown agent: err=%v, want ErrAgentNotFound", err)
	}
}
