package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/logger"
)

// newReportServer returns a server with a "demo" agent, ReportsKeep=5, the
// report API and the rendered page mounted on one mux, and the agent's
// reports directory.
func newReportServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	s, ar := newAgentServer(t, nil)
	s.log = logger.Nop()
	s.cfg.ReportsKeep = 5
	mux := newTestAPI(t, s.registerReportRoutes)
	mux.HandleFunc("GET /reports/{key}/{name}", s.handleReportPage)
	dir := filepath.Join(ar.Root(), "demo", "workspace", agents.ReportsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return mux, dir
}

func putReport(t *testing.T, dir, name, content string, mod time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func getPath(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestReportListAPI(t *testing.T) {
	h, dir := newReportServer(t)
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	putReport(t, dir, "older.md", "old", base)
	putReport(t, dir, "weekly report.md", "new", base.Add(time.Hour))

	rec := getPath(h, "/api/agents/demo/reports")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Keep    int `json:"keep"`
		Reports []struct {
			Name     string    `json:"name"`
			Size     int64     `json:"size"`
			Modified time.Time `json:"modified"`
			URL      string    `json:"url"`
			RawURL   string    `json:"raw_url"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Keep != 5 || len(got.Reports) != 2 {
		t.Fatalf("list = %+v", got)
	}
	first := got.Reports[0]
	if first.Name != "weekly report.md" || first.Size != 3 || !first.Modified.Equal(base.Add(time.Hour)) {
		t.Fatalf("newest report = %+v", first)
	}
	if first.URL != "/reports/demo/weekly%20report.md" || first.RawURL != "/api/agents/demo/reports/weekly%20report.md" {
		t.Fatalf("links = %q, %q", first.URL, first.RawURL)
	}
	// The advertised links resolve.
	for _, link := range []string{first.URL, first.RawURL} {
		if rec := getPath(h, link); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", link, rec.Code)
		}
	}

	if rec := getPath(h, "/api/agents/ghost/reports"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent status = %d", rec.Code)
	}
}

func TestReportRawAPI(t *testing.T) {
	h, dir := newReportServer(t)
	putReport(t, dir, "page.html", "<script>alert(1)</script>", time.Now())

	rec := getPath(h, "/api/agents/demo/reports/page.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain even for .html", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if rec.Body.String() != "<script>alert(1)</script>" {
		t.Fatalf("body = %q", rec.Body)
	}

	tests := []struct {
		path       string
		wantStatus int
		wantDetail string
	}{
		{"/api/agents/demo/reports/missing.md", http.StatusNotFound, "only the latest 5 reports are kept"},
		{"/api/agents/demo/reports/.hidden.md", http.StatusBadRequest, "invalid report name"},
		{"/api/agents/ghost/reports/page.html", http.StatusNotFound, "agent not found"},
	}
	for _, tt := range tests {
		rec := getPath(h, tt.path)
		if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), tt.wantDetail) {
			t.Errorf("GET %s = %d %s, want %d containing %q", tt.path, rec.Code, rec.Body, tt.wantStatus, tt.wantDetail)
		}
	}
}

func TestReportPageRendersMarkdownSafely(t *testing.T) {
	h, dir := newReportServer(t)
	report := strings.Join([]string{
		"# Security audit",
		"",
		"| Workload | Restarts |",
		"|---|---|",
		"| api-worker | 105 |",
		"",
		"<script>alert('raw')</script>",
		"",
		`<img src="x" onerror="alert('img')">`,
		"",
		"[bad link](javascript:alert('link')) and [grafana](https://grafana.example)",
	}, "\n")
	putReport(t, dir, "audit.md", report, time.Date(2026, 11, 19, 10, 2, 0, 0, time.UTC))

	rec := getPath(h, "/reports/demo/audit.md")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP = %q", csp)
	}
	body := rec.Body.String()
	for _, want := range []string{"<h1>Security audit</h1>", "<table>", "<td>105</td>", `href="https://grafana.example"`, "Updated 2026-11-19 10:02 UTC", `href="/api/agents/demo/reports/audit.md"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, bad := range []string{"<script", "onerror", "javascript:"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q:\n%s", bad, body)
		}
	}

	// The only inline style is the one the CSP allows by hash.
	start, end := strings.Index(body, "<style>"), strings.Index(body, "</style>")
	if start < 0 || end < start {
		t.Fatal("no style block")
	}
	sum := sha256.Sum256([]byte(body[start+len("<style>") : end]))
	if hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, hash) {
		t.Fatalf("CSP %q does not allow the page's style block (%s)", csp, hash)
	}
}

func TestReportPagePlainTextAndErrors(t *testing.T) {
	h, dir := newReportServer(t)
	putReport(t, dir, "notes.txt", "<b>not bold</b>", time.Now())

	rec := getPath(h, "/reports/demo/notes.txt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<pre>&lt;b&gt;not bold&lt;/b&gt;</pre>") {
		t.Fatalf("plain text page = %d %s", rec.Code, rec.Body)
	}

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/reports/demo/missing.md", http.StatusNotFound, "only the latest 5 reports are kept"},
		{"/reports/demo/.hidden.md", http.StatusBadRequest, "invalid report name"},
		{"/reports/ghost/notes.txt", http.StatusNotFound, "agent not found"},
	}
	for _, tt := range tests {
		rec := getPath(h, tt.path)
		if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), tt.wantBody) {
			t.Errorf("GET %s = %d %s, want %d containing %q", tt.path, rec.Code, rec.Body, tt.wantStatus, tt.wantBody)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("GET %s Content-Type = %q, want text/plain", tt.path, ct)
		}
	}
}
