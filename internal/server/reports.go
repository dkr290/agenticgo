package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/dkr290/agenticgo/internal/agents"
)

// reportMarkdown renders report Markdown (GitHub flavor: tables, task lists,
// autolinks). goldmark's default renderer is safe: raw HTML is dropped and
// javascript:/vbscript:/data: links are neutralized.
var reportMarkdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// reportCSS styles the rendered page. It is the page's only inline content
// and is allowed by hash, so the CSP needs no 'unsafe-inline' at all.
const reportCSS = `:root{color-scheme:light dark;--bg:#fff;--fg:#1f2328;--muted:#656d76;--border:#d0d7de;--code:#f6f8fa;--link:#0969da}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--muted:#8d96a0;--border:#30363d;--code:#161b22;--link:#4493f8}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif}
header{display:flex;flex-wrap:wrap;justify-content:space-between;gap:4px 16px;padding:12px 24px;border-bottom:1px solid var(--border);font-size:13px;color:var(--muted)}
header strong{color:var(--fg)}
main{max-width:1100px;margin:0 auto;padding:8px 24px 48px}
a{color:var(--link)}
h1,h2,h3,h4{line-height:1.25;margin:1.4em 0 .6em}
h1,h2{padding-bottom:.3em;border-bottom:1px solid var(--border)}
h1{font-size:1.7em}
h2{font-size:1.35em}
table{display:block;overflow-x:auto;border-collapse:collapse;margin:1em 0}
th,td{border:1px solid var(--border);padding:6px 12px;text-align:left;vertical-align:top}
th{background:var(--code)}
code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.9em;background:var(--code);border-radius:6px}
code{padding:.15em .35em}
pre{padding:12px 16px;overflow-x:auto;white-space:pre-wrap;word-break:break-word}
pre code{padding:0;background:none}
blockquote{margin:1em 0;padding:0 1em;color:var(--muted);border-left:4px solid var(--border)}
li{margin:.25em 0}`

// reportPageCSP blocks scripts, images, frames, forms and every other
// resource: an agent-written report can only ever display text.
var reportPageCSP = func() string {
	sum := sha256.Sum256([]byte(reportCSS))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

var reportPageTmpl = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} · {{.Agent}}</title>
<style>{{.CSS}}</style>
</head>
<body>
<header>
<div><strong>{{.Agent}}</strong> · {{.Name}}</div>
<div>Updated {{.Modified}} · <a href="{{.RawURL}}">Raw</a> · <a href="/">agenticgo</a></div>
</header>
<main>
{{if .Plain}}<pre>{{.Text}}</pre>{{else}}{{.HTML}}{{end}}
</main>
</body>
</html>
`))

type reportPageData struct {
	Agent, Name, Modified, RawURL string
	CSS                           template.CSS
	HTML                          template.HTML
	Text                          string
	Plain                         bool
}

// reportPageURL is the shareable, rendered page of a report.
func reportPageURL(key, name string) string {
	return "/reports/" + url.PathEscape(key) + "/" + url.PathEscape(name)
}

// reportRawURL is the raw-content API endpoint of a report.
func reportRawURL(key, name string) string {
	return "/api/agents/" + url.PathEscape(key) + "/reports/" + url.PathEscape(name)
}

// reportStatus maps a report lookup failure to an HTTP status and message.
func (s *Server) reportStatus(err error) (int, string) {
	switch {
	case errors.Is(err, agents.ErrInvalidReportName):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, agents.ErrAgentNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, agents.ErrReportNotFound):
		if s.cfg.ReportsKeep > 0 {
			return http.StatusNotFound, fmt.Sprintf("report not found — only the latest %d reports are kept per agent", s.cfg.ReportsKeep)
		}
		return http.StatusNotFound, "report not found"
	default:
		return http.StatusInternalServerError, err.Error()
	}
}

// reportError is reportStatus as a Huma error for the REST operations.
func (s *Server) reportError(err error) error {
	status, msg := s.reportStatus(err)
	return huma.NewError(status, msg)
}

// handleReportPage serves GET /reports/{key}/{name}: one report as a
// standalone HTML page — the link agents share in notifications. Markdown is
// rendered with goldmark's safe defaults and the CSP blocks scripts, so
// agent-written content cannot run on the agenticgo origin. Other file types
// are shown as preformatted text.
func (s *Server) handleReportPage(w http.ResponseWriter, r *http.Request) {
	key, name := r.PathValue("key"), r.PathValue("name")
	h := w.Header()
	h.Set("Content-Security-Policy", reportPageCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")

	data, info, err := s.agents.ReadReport(key, name)
	if err != nil {
		status, msg := s.reportStatus(err)
		if status >= http.StatusInternalServerError {
			s.log.Error("report page failed", "agent", key, "report", name, "error", err)
			msg = "could not read the report"
		}
		http.Error(w, msg, status)
		return
	}

	page := reportPageData{
		Agent:    key,
		Name:     info.Name,
		Modified: info.Modified.Format("2006-01-02 15:04 UTC"),
		RawURL:   reportRawURL(key, info.Name),
		CSS:      template.CSS(reportCSS),
	}
	switch strings.ToLower(filepath.Ext(info.Name)) {
	case ".md", ".markdown":
		var md bytes.Buffer
		if err := reportMarkdown.Convert(data, &md); err != nil {
			s.log.Error("report render failed", "agent", key, "report", name, "error", err)
			http.Error(w, "could not render the report", http.StatusInternalServerError)
			return
		}
		page.HTML = template.HTML(md.String()) // safe: goldmark drops raw HTML
	default:
		page.Text, page.Plain = string(data), true
	}

	var out bytes.Buffer
	if err := reportPageTmpl.Execute(&out, page); err != nil {
		s.log.Error("report page template failed", "agent", key, "report", name, "error", err)
		http.Error(w, "could not render the report", http.StatusInternalServerError)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out.Bytes())
}
