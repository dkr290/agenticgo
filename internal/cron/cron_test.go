package cron

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cron.json"), func(context.Context, string, string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("expected no jobs, got %d", len(got))
	}
}

func TestAddValidation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cron.json"), func(context.Context, string, string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		job     Job
		wantErr bool
	}{
		{"missing name", Job{Schedule: "* * * * *", Prompt: "x"}, true},
		{"bad schedule", Job{Name: "a", Schedule: "not-a-schedule", Prompt: "x"}, true},
		{"missing prompt", Job{Name: "a", Schedule: "* * * * *", Prompt: "   "}, true},
		{"valid, agent defaults", Job{Name: "a", Schedule: "* * * * *", Prompt: "x"}, false},
	}
	for _, c := range cases {
		_, err := s.Add(c.job)
		if c.wantErr && err == nil {
			t.Errorf("%s: expected error, got none", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
		}
	}
	if got := s.List(); len(got) != 1 {
		t.Fatalf("expected 1 valid job, got %d", len(got))
	}
	if got := s.List()[0]; got.Agent != "default" {
		t.Fatalf("expected empty agent to default to 'default', got %q", got.Agent)
	}
}

func TestAddPersistsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.json")
	run := func(context.Context, string, string, string) error { return nil }

	s, err := Open(path, run)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Add(Job{Name: "watch", Schedule: "*/5 * * * *", Prompt: "check", Agent: "k8s", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if j.ID == "" {
		t.Fatal("expected an ID to be assigned")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cron.json not written: %v", err)
	}

	s2, err := Open(path, run)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.List(); len(got) != 1 || got[0].Name != "watch" || got[0].ID != j.ID {
		t.Fatalf("reopen did not round-trip job: %+v", got)
	}
}

func TestDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.json")
	run := func(context.Context, string, string, string) error { return nil }
	s, err := Open(path, run)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Add(Job{Name: "x", Schedule: "* * * * *", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(j.ID); err == nil {
		t.Fatal("expected error deleting a missing job")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var list []*Job
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty file after delete, got %d", len(list))
	}
}

func TestEnabledArmsAndDisabledDoesNot(t *testing.T) {
	var fired int
	s, err := Open(filepath.Join(t.TempDir(), "cron.json"), func(context.Context, string, string, string) error {
		fired++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Job{Name: "on", Schedule: "* * * * *", Prompt: "p", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	off, err := s.Add(Job{Name: "off", Schedule: "* * * * *", Prompt: "p", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop()

	byName := map[string]JobWithStatus{}
	for _, j := range s.List() {
		byName[j.Name] = j
	}
	if !byName["on"].Scheduled {
		t.Fatal("enabled job should be armed")
	}
	if byName["off"].Scheduled {
		t.Fatal("disabled job must not be armed")
	}

	// Smoke-test the fire path: the runner is invoked.
	s.fire(off.ID, off.Agent, off.Prompt)
	if fired != 1 {
		t.Fatalf("expected fire to invoke runner once, got %d", fired)
	}
}
