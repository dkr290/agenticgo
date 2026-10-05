package cron

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSkipOverlappingRunsAndStopCancels(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, err := Open(filepath.Join(t.TempDir(), "cron.json"), func(ctx context.Context, _, _, _ string) error {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop()
	go s.fire("job", "demo", "wait")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not start")
	}
	s.fire("job", "demo", "overlap")
	if calls.Load() != 1 {
		t.Fatal("overlapping job ran")
	}
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel/wait for running job")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before runner stopped")
	}
	s.fire("job", "demo", "after stop")
	if calls.Load() != 1 {
		t.Fatal("job ran after Stop")
	}
}
