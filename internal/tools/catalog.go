package tools

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/microsoft/agent-framework-go/tool"
)

// BuiltinToolInfo is the name/description metadata of one allow-listed
// built-in tool, for the Built-in Tools UI page.
type BuiltinToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// BuiltinTools returns metadata for the built-in workspace tools gated by the
// global allow-list (AGENTICGO_TOOL_ALLOWLIST ceiling). It powers the
// read-only Built-in Tools API/UI. Tools are constructed over the process
// temporary directory — only their metadata is read; they are never invoked
// here (per-run instances are jailed to each agent's own workspace).
func BuiltinTools(allowList []string) []BuiltinToolInfo {
	allowed := map[string]bool{}
	for _, n := range allowList {
		allowed[n] = true
	}
	ok := func(name string) bool { return len(allowed) == 0 || allowed[name] }

	dir := tempDirForCatalog()
	var out []BuiltinToolInfo
	add := func(t tool.Tool, err error) {
		if err == nil && t != nil && ok(t.Name()) {
			out = append(out, BuiltinToolInfo{Name: t.Name(), Description: t.Description()})
		}
	}
	if ok("read_file") {
		t, err := NewReadFile(dir)
		add(t, err)
	}
	if ok("write_file") {
		t, err := NewWriteFile(dir)
		add(t, err)
	}
	if ok("list_files") {
		t, err := NewListFiles(dir)
		add(t, err)
	}
	if ok("exec") {
		add(NewExec(dir, nil), nil)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// tempDirForCatalog returns a scratch directory for catalog-only tool
// construction (metadata is read; the tools are never invoked).
func tempDirForCatalog() string {
	return filepath.Join(os.TempDir(), "agenticgo-tool-catalog")
}
