package agenttemplates

import (
	"strings"
	"testing"
)

// TestRenderAllTemplates ensures every embedded context-file template parses
// and renders without error.
func TestRenderAllTemplates(t *testing.T) {
	names := []string{
		"AGENTS.md",
		"SOUL.md",
		"IDENTITY.md",
		"USER.md",
		"USER_PREDEFINED.md",
		"CAPABILITIES.md",
		"HEARTBEAT.md",
	}
	data := Data{Name: "Demo", Role: "test agent"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if _, err := Render(name, data); err != nil {
				t.Fatalf("Render(%q): %v", name, err)
			}
		})
	}
}

func TestRenderSubstitutesVariables(t *testing.T) {
	data := Data{Name: "Freyja", Role: "Norse researcher"}

	soul, err := Render("SOUL.md", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(soul, "You are Freyja,") {
		t.Fatalf("SOUL.md missing name substitution:\n%s", soul)
	}
	if strings.Contains(soul, "{{.Name}}") {
		t.Fatalf("SOUL.md still has placeholder:\n%s", soul)
	}

	id, err := Render("IDENTITY.md", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(id, "**Name:** Freyja") || !strings.Contains(id, "**Role:** Norse researcher") {
		t.Fatalf("IDENTITY.md missing name/role substitution:\n%s", id)
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	if _, err := Render("NOPE.md", Data{}); err == nil {
		t.Fatal("expected error for unknown template")
	}
}

func TestRenderVerbatimTemplatesHaveNoPlaceholders(t *testing.T) {
	// Templates without variables must render byte-identical to their source.
	for _, name := range []string{"AGENTS.md", "USER.md", "USER_PREDEFINED.md", "CAPABILITIES.md", "HEARTBEAT.md"} {
		out, err := Render(name, Data{Name: "X", Role: "Y"})
		if err != nil {
			t.Fatalf("Render(%q): %v", name, err)
		}
		if strings.Contains(out, "{{") {
			t.Fatalf("%s unexpectedly contains a placeholder:\n%s", name, out)
		}
	}
}
