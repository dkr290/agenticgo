// Package skills loads per-agent skills. A skill is a directory containing a
// SKILL.md file with instructions the agent can apply, following the
// Anthropic/OpenClaw "skill" convention:
//
//	agents/researcher/skills/
//	  web-research/
//	    SKILL.md     # front-matter (name, description) + instructions
//	    helper.py    # optional supporting files (informational)
//
// Skills are injected into the system prompt (progressive disclosure: name +
// description always, full instructions when enabled) so the model knows when
// and how to use them.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is a single loaded skill.
type Skill struct {
	Key         string   `json:"key"`         // directory name
	Name        string   `json:"name"`        // from front-matter, defaults to key
	Description string   `json:"description"` // from front-matter
	Body        string   `json:"body"`        // instructions (markdown after front-matter)
	Files       []string `json:"files"`       // supporting files in the skill dir
}

// Load reads all skills under a skills directory.
func Load(skillsDir string) ([]Skill, error) {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills dir: %w", err)
	}

	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sk, err := loadOne(filepath.Join(skillsDir, e.Name()), e.Name())
		if err != nil {
			continue // skip malformed skills
		}
		out = append(out, *sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func loadOne(dir, key string) (*Skill, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return nil, fmt.Errorf("skill %q has no SKILL.md", key)
	}
	sk := &Skill{Key: key, Name: key}
	sk.Name, sk.Description, sk.Body = parseSKILL(string(data), key)

	// Record supporting files (informational).
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && e.Name() != "SKILL.md" {
				sk.Files = append(sk.Files, e.Name())
			}
		}
	}
	return sk, nil
}

// parseSKILL parses a SKILL.md into name, description, and body.
// Supports an optional YAML-ish front-matter block:
//
//	---
//	name: Web Research
//	description: Search and synthesize web sources
//	---
//	# Instructions...
func parseSKILL(content, key string) (name, description, body string) {
	name = key
	content = strings.TrimPrefix(content, "\uFEFF") // strip BOM
	lines := strings.Split(content, "\n")

	bodyStart := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			l := strings.TrimSpace(lines[i])
			if l == "---" {
				bodyStart = i + 1
				break
			}
			if k, v, ok := strings.Cut(l, ":"); ok {
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "name":
					if s := strings.TrimSpace(v); s != "" {
						name = s
					}
				case "description":
					if s := strings.TrimSpace(v); s != "" {
						description = s
					}
				}
			}
		}
	}
	if bodyStart < len(lines) {
		body = strings.TrimSpace(strings.Join(lines[bodyStart:], "\n"))
	}
	return name, description, body
}

// Prompt renders skills as a system-prompt section. If enabled is empty, all
// skills are included; otherwise only the named skill keys.
func Prompt(skills []Skill, enabled []string) string {
	if len(skills) == 0 {
		return ""
	}
	allow := map[string]bool{}
	for _, e := range enabled {
		allow[e] = true
	}
	filter := len(enabled) > 0

	var b strings.Builder
	b.WriteString("## Skills\n")
	b.WriteString("You have the following skills. Apply them when relevant to the task.\n\n")
	n := 0
	for _, sk := range skills {
		if filter && !allow[sk.Key] {
			continue
		}
		n++
		fmt.Fprintf(&b, "### Skill: %s\n", sk.Name)
		if sk.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", sk.Description)
		}
		if sk.Body != "" {
			b.WriteString(sk.Body)
			b.WriteString("\n\n")
		}
	}
	if n == 0 {
		return ""
	}
	return strings.TrimSpace(b.String())
}
