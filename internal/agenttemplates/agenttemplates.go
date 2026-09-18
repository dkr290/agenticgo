// Package agenttemplates holds the initial content for an agent's context
// files (AGENTS.md, SOUL.md, IDENTITY.md, USER.md, USER_PREDEFINED.md,
// CAPABILITIES.md, HEARTBEAT.md), embedded into the binary.
//
// The templates are the *initial* content only: Registry.Create renders them
// into data/agents/<key>/ when an agent is first created; after that the files
// live on disk and are edited through the GUI. Two templates carry variables
// (SOUL.md uses {{.Name}}; IDENTITY.md uses {{.Name}} and {{.Role}}) which are
// substituted at render time.
package agenttemplates

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed all:templates
var templatesFS embed.FS

// Data carries the variables substituted into a template at render time.
type Data struct {
	Name string // agent display name (SOUL.md, IDENTITY.md)
	Role string // agent role / short description (IDENTITY.md)
}

// Render reads templates/<name> from the embedded FS and executes it with
// data. Files without placeholders render verbatim. Errors are wrapped with
// the template name.
func Render(name string, data Data) (string, error) {
	raw, err := templatesFS.ReadFile("templates/" + name)
	if err != nil {
		return "", fmt.Errorf("read template %s: %w", name, err)
	}
	tpl, err := template.New(name).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	return buf.String(), nil
}
