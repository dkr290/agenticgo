package agents

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReportsDirName is the workspace sub-directory agents write reports to.
// Its files are published read-only (raw API + rendered page, so a
// notification can link to the full report) and pruned to the newest N.
const ReportsDirName = "reports"

// MaxReportBytes caps how much of a single report is read for serving.
const MaxReportBytes = 5 << 20 // 5 MB

var (
	// ErrAgentNotFound is returned when the agent does not exist.
	ErrAgentNotFound = errors.New("agent not found")
	// ErrReportNotFound is returned when a report does not exist — never
	// written, or already removed by retention.
	ErrReportNotFound = errors.New("report not found")
	// ErrInvalidReportName is returned for anything but a plain file name.
	ErrInvalidReportName = errors.New("invalid report name")
)

// Report describes one file in an agent's reports directory.
type Report struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// ValidReportName reports whether name is a single plain file name that is
// safe to serve: no path separators or control characters, and not hidden
// (which also rules out "." and "..").
func ValidReportName(name string) bool {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.ContainsFunc(name, func(c rune) bool {
		return c == '/' || c == '\\' || c < 0x20 || c == 0x7f
	})
}

// ListReports returns the agent's reports, newest first. Only regular files
// directly inside the reports directory count; directories, symlinks and
// hidden files are ignored. An agent that has not written a report yet has
// an empty list.
func (r *Registry) ListReports(key string) ([]Report, error) {
	root, err := r.openReports(key)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return []Report{}, nil
	}
	defer root.Close()
	return listReports(root)
}

// ReadReport returns one report's content and metadata. Only regular files
// are served: a symlink or directory reads as not found, and the reports
// directory is opened through the workspace root, so nothing can resolve
// outside the agent's workspace.
func (r *Registry) ReadReport(key, name string) ([]byte, Report, error) {
	if !ValidReportName(name) {
		return nil, Report{}, fmt.Errorf("%w: %q", ErrInvalidReportName, name)
	}
	root, err := r.openReports(key)
	if err != nil {
		return nil, Report{}, err
	}
	if root == nil {
		return nil, Report{}, fmt.Errorf("%w: %q", ErrReportNotFound, name)
	}
	defer root.Close()

	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return nil, Report{}, fmt.Errorf("%w: %q", ErrReportNotFound, name)
	}
	if err != nil {
		return nil, Report{}, fmt.Errorf("stat report: %w", err)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, Report{}, fmt.Errorf("open report: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxReportBytes+1))
	if err != nil {
		return nil, Report{}, fmt.Errorf("read report: %w", err)
	}
	if len(data) > MaxReportBytes {
		return nil, Report{}, fmt.Errorf("report %q is larger than %d MB", name, MaxReportBytes>>20)
	}
	return data, Report{Name: name, Size: info.Size(), Modified: info.ModTime().UTC()}, nil
}

// PruneReports deletes all but the newest keep reports of an agent (by
// modification time) and returns how many were removed. keep <= 0 keeps
// everything.
func (r *Registry) PruneReports(key string, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	root, err := r.openReports(key)
	if err != nil || root == nil {
		return 0, err
	}
	defer root.Close()
	reports, err := listReports(root)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, rp := range reports[min(keep, len(reports)):] {
		if err := root.Remove(rp.Name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("remove report %q: %w", rp.Name, err)
		}
		removed++
	}
	return removed, nil
}

// openReports opens the agent's reports directory as an os.Root nested in its
// workspace root. It returns (nil, nil) when the directory doesn't exist yet.
func (r *Registry) openReports(key string) (*os.Root, error) {
	if !r.Exists(key) {
		return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, key)
	}
	dir, err := r.dirFor(key)
	if err != nil {
		return nil, err
	}
	ws, err := os.OpenRoot(filepath.Join(dir, "workspace"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open workspace: %w", err)
	}
	defer ws.Close()
	root, err := ws.OpenRoot(ReportsDirName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open reports dir: %w", err)
	}
	return root, nil
}

// listReports lists the regular, non-hidden files of root, newest first.
func listReports(root *os.Root) ([]Report, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("read reports dir: %w", err)
	}
	out := []Report{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !ValidReportName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed concurrently
		}
		out = append(out, Report{Name: e.Name(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Modified.Equal(out[j].Modified) {
			return out[i].Modified.After(out[j].Modified)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}
