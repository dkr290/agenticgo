package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
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

// reportPageFS holds the rendered report page — an html/template and its
// stylesheet — as plain files embedded like web/, but not served directly.
//
//go:embed reportpage
var reportPageFS embed.FS

var (
	reportPageTmpl = template.Must(template.ParseFS(reportPageFS, "reportpage/report.html"))
	// reportCSS is the page's only inline content; it is allowed by hash, so
	// the CSP needs no 'unsafe-inline' at all.
	reportCSS = mustReadEmbedded(reportPageFS, "reportpage/report.css")
	// reportPageCSP blocks scripts, images, frames, forms and every other
	// resource: an agent-written report can only ever display text.
	reportPageCSP = "default-src 'none'; style-src '" + cspHash(reportCSS) +
		"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

func mustReadEmbedded(fsys embed.FS, name string) string {
	b, err := fsys.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("embedded %s: %v", name, err))
	}
	return string(b)
}

// cspHash is the CSP source expression allowing exactly this inline content.
func cspHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

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
