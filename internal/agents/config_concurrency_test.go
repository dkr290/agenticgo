package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentConfigMutations(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create("demo", "demo", "", "", AgentConfig{EnabledBuiltinTools: &[]string{}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			if _, err := r.MutateConfig("demo", func(cfg *AgentConfig) error {
				cfg.EnabledTools = append(cfg.EnabledTools, fmt.Sprintf("tool-%d", i))
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	ag, err := r.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Config.EnabledTools) != 24 || ag.Config.EnabledBuiltinTools == nil || len(*ag.Config.EnabledBuiltinTools) != 0 {
		t.Fatalf("lost config: %+v", ag.Config)
	}
}

func TestCorruptConfigDoesNotInheritPermissions(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create("demo", "demo", "", "", AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.Root(), "demo", "config.json")
	if err := os.WriteFile(path, []byte(`{"enabled_builtin_tools":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("demo"); err == nil {
		t.Fatal("corrupt config silently inherited defaults")
	}
	if _, err := r.SetEnabledSkills("demo", []string{"skill"}); err == nil {
		t.Fatal("mutation overwrote corrupt config")
	}
}
