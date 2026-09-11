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
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

// slugRE validates skill slugs (used as directory names).
var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// slugify derives a filesystem-safe slug from a display name.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '_', r == '-':
			b.WriteByte('-')
		}
	}
	// Collapse repeats and trim.
	slug := slugRE.FindString(strings.Trim(b.String(), "-"))
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	return slug
}

// systemArtifacts are filenames never extracted from an uploaded ZIP.
var systemArtifacts = map[string]bool{
	".ds_store": true, "__macosx": true, "thumbs.db": true, ".git": true,
}

// InstallZip installs a skill from a ZIP archive into destDir/<slug>/.
//
// The archive must contain a SKILL.md (at the root or inside a single
// top-level folder). SKILL.md must have a `name` in its front-matter; the slug
// is derived from it (or the `slug` field). Extraction is path-traversal-safe
// and skips symlinks and OS artifacts. Returns the parsed skill.
func InstallZip(destDir string, data []byte) (*Skill, error) {
	const maxUncompressed = 50 << 20 // 50 MB safety cap
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}

	// Find SKILL.md, allowing a single top-level wrapper folder.
	var skillEntry *zip.File
	prefix := ""
	for _, f := range zr.File {
		name := strings.TrimPrefix(filepath.ToSlash(f.Name), "./")
		if systemArtifacts[strings.ToLower(base(name))] {
			continue
		}
		base := base(name)
		if strings.EqualFold(base, "SKILL.md") {
			skillEntry = f
			prefix = strings.TrimSuffix(name, base) // "" or "folder/"
			break
		}
	}
	if skillEntry == nil {
		return nil, fmt.Errorf("zip does not contain a SKILL.md")
	}

	// Parse SKILL.md to get name/slug before writing anything.
	skillData, err := readZipFile(skillEntry, maxUncompressed)
	if err != nil {
		return nil, err
	}
	name, description, _ := parseSKILL(string(skillData), "")
	slug := slugify(frontMatterValue(string(skillData), "slug"))
	if slug == "" {
		slug = slugify(name)
	}
	if slug == "" || !slugRE.MatchString(slug) {
		return nil, fmt.Errorf("SKILL.md needs a valid 'name' (or 'slug') in front-matter")
	}

	target := filepath.Join(destDir, slug)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("create skill dir: %w", err)
	}

	// Extract all files under the SKILL.md's folder, safely.
	for _, f := range zr.File {
		name := strings.TrimPrefix(filepath.ToSlash(f.Name), "./")
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
			continue // path traversal guard
		}
		if systemArtifacts[strings.ToLower(base(rel))] {
			continue
		}
		dest := filepath.Join(target, filepath.FromSlash(rel))
		if !strings.HasPrefix(dest, filepath.Clean(target)+string(os.PathSeparator)) && dest != target {
			continue // outside target
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		// Skip symlinks.
		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		content, err := readZipFile(f, maxUncompressed)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(dest, content, 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", rel, err)
		}
	}

	sk := &Skill{Key: slug, Name: name, Description: description}
	return sk, nil
}

func base(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func readZipFile(f *zip.File, max int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, max))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	return data, nil
}

// frontMatterValue returns a single front-matter value by key.
func frontMatterValue(content, key string) string {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for i := 1; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if l == "---" {
			break
		}
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Delete removes a skill directory from the library, validating the key.
func Delete(skillsDir, key string) error {
	if !slugRE.MatchString(key) {
		return fmt.Errorf("invalid skill key %q", key)
	}
	dir := filepath.Join(skillsDir, key)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("skill %q not found", key)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete skill %q: %w", key, err)
	}
	return nil
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
