package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

type schedulerPaseo struct {
	started chan string
	fail    chan struct{}
}

func (p schedulerPaseo) Inspect(ctx context.Context, id, _ string) (*paseo.Object, error) {
	p.started <- id
	if id == "slow" {
		select {
		case <-p.fail:
		case <-ctx.Done():
		}
		return nil, paseo.ErrCommandUnavailable
	}
	return paseo.Obj("Id", id, "Archived", true), nil
}

func (schedulerPaseo) Send(context.Context, string, string, string, string) error {
	return errors.New("unexpected send")
}

func TestPaseoSchedulerRetriesWithoutBlockingOtherRegistrations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	engine := paseo.New(&config.Config{Paseo: config.Paseo{RegistrationsDir: dir}}, nil)
	fake := schedulerPaseo{started: make(chan string, 4), fail: make(chan struct{})}
	engine.Paseo = fake
	for _, id := range []string{"slow", "healthy"} {
		state := paseo.Obj("enabled", true, "home", dir, "agent_id", id, "poll", 300)
		if err := paseo.AtomicWrite(filepath.Join(dir, id+".json"), state); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	s := newPaseoScheduler(engine, log.New(&output, "", 0))
	path := filepath.Join(dir, "slow.json")
	s.round(ctx, paseoKick{})
	waitStart := func() string {
		t.Helper()
		select {
		case id := <-fake.started:
			return id
		case <-ctx.Done():
			t.Fatal("check did not start")
			return ""
		}
	}
	first, second := waitStart(), waitStart()
	if first == second {
		t.Fatalf("started %q twice", first)
	}
	// The healthy check must finish before the slow agent times out.
	healthyPath := filepath.Join(dir, "healthy.json")
	for {
		s.mu.Lock()
		running := s.checks[healthyPath].running
		s.mu.Unlock()
		if !running {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("healthy check stalled behind slow agent")
		case <-time.After(time.Millisecond):
		}
	}
	close(fake.fail)
	waitRetry := func() time.Time {
		t.Helper()
		for {
			s.mu.Lock()
			check := s.checks[path]
			at, running := check.retry.At, check.running
			s.mu.Unlock()
			if !running && !at.IsZero() {
				return at
			}
			select {
			case <-ctx.Done():
				t.Fatal("retry was not scheduled")
				return time.Time{}
			case <-time.After(time.Millisecond):
			}
		}
	}
	wantAt := waitRetry()
	if wait := time.Until(wantAt); wait < 15*time.Second || wait > 20*time.Second {
		t.Fatalf("first retry waits %s", wait)
	}
	s.round(ctx, paseoKick{path: path})
	s.mu.Lock()
	if s.checks[path].running {
		s.mu.Unlock()
		t.Fatal("explicit kick bypassed backoff")
	}
	s.checks[path].retry.At = time.Now().Add(-time.Second)
	s.checks[path].next = s.checks[path].retry.At
	s.mu.Unlock()
	s.round(ctx, paseoKick{})
	if id := waitStart(); id != "slow" {
		t.Fatalf("retried %s", id)
	}
	if wait := time.Until(waitRetry()); wait < 35*time.Second || wait > 40*time.Second {
		t.Fatalf("second retry waits %s", wait)
	}
	s.mu.Lock()
	logs := output.String()
	s.mu.Unlock()
	if got := strings.Count(logs, "check unavailable"); got != 1 {
		t.Fatalf("logged %d check failures: %s", got, logs)
	}
}

// An agy agent waiting for quota is checked once at the reset its notice
// names (wake_at), well before its safety tick.
func TestPaseoSchedulerWakesAgyAgentAtReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	engine := paseo.New(&config.Config{Paseo: config.Paseo{RegistrationsDir: dir}}, nil)
	fake := schedulerPaseo{started: make(chan string, 4), fail: make(chan struct{})}
	engine.Paseo = fake
	path := filepath.Join(dir, "agy.json")
	register := func(wakeAt float64) {
		t.Helper()
		state := paseo.Obj("enabled", true, "home", dir, "agent_id", "agy", "poll", 300,
			"status", paseo.StatusWaitingQuota, "wake_at", wakeAt)
		if err := paseo.AtomicWrite(path, state); err != nil {
			t.Fatal(err)
		}
	}
	s := newPaseoScheduler(engine, log.New(&bytes.Buffer{}, "", 0))
	settle := func() {
		t.Helper()
		for {
			s.mu.Lock()
			running := s.checks[path] != nil && s.checks[path].running
			s.mu.Unlock()
			if !running {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("check stalled")
			case <-time.After(time.Millisecond):
			}
		}
	}
	started := func() bool {
		select {
		case <-fake.started:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}
	future := float64(time.Now().Add(time.Hour).Unix())
	register(future)
	s.round(ctx, paseoKick{}) // the first round checks every registration
	if !started() {
		t.Fatal("first check did not start")
	}
	settle()
	register(future)
	s.round(ctx, paseoKick{})
	if started() {
		t.Fatal("checked before the reset")
	}
	past := float64(time.Now().Add(-time.Second).Unix())
	register(past)
	s.round(ctx, paseoKick{})
	if !started() {
		t.Fatal("not checked at the reset")
	}
	settle()
	register(past)
	s.round(ctx, paseoKick{})
	if started() {
		t.Fatal("one reset woke two checks")
	}
}
