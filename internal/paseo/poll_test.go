package paseo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type pollingPaseo struct {
	*fakePaseo
	inspects int
	listings int
	items    []*Object
	err      error
}

func (p *pollingPaseo) Inspect(ctx context.Context, id, home string) (*Object, error) {
	p.inspects++
	if p.err != nil {
		return nil, p.err
	}
	return p.fakePaseo.Inspect(ctx, id, home)
}

func (p *pollingPaseo) List(context.Context, string) ([]*Object, error) {
	p.listings++
	return p.items, nil
}

func TestPollUsesFileUntilFailureIsPlausible(t *testing.T) {
	h := newHarness(t)
	h.e.storedFn = nil
	p := &pollingPaseo{fakePaseo: h.paseo}
	h.e.Paseo = p
	dir := filepath.Join(h.home, "agents", "project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "agent-id.json")
	h.record.Set("provider", h.provider)
	h.record.Set("cwd", "/tmp/project")
	h.record.Set("lastStatus", "running")
	if err := atomicWrite(file, h.record); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if status, err := h.e.Poll(ctx, h.path, &PollListing{}); err != nil || status != StatusWaitingFailure {
		t.Fatalf("running file: status %q, error %v", status, err)
	}
	if p.inspects != 0 || p.listings != 0 || len(p.sent) != 0 {
		t.Fatalf("running file used Paseo: %+v", p)
	}

	h.record.Set("lastStatus", "error")
	if err := atomicWrite(file, h.record); err != nil {
		t.Fatal(err)
	}
	p.err = ErrCommandUnavailable
	if _, err := h.e.Poll(ctx, h.path, &PollListing{}); !errors.Is(err, ErrCommandUnavailable) {
		t.Fatalf("failed live check: %v", err)
	}
	p.err = nil
	fresh := h.snapshot.Clone()
	fresh.Set("PendingPermissions", []any{Obj("id", "p1")})
	p.snapshots = []*Object{h.snapshot, fresh}
	if status, err := h.e.Poll(ctx, h.path, &PollListing{}); err != nil || status != StatusChanged {
		t.Fatalf("changed live state: status %q, error %v", status, err)
	}
	if p.inspects != 3 || p.listings != 0 || len(p.sent) != 0 {
		t.Fatalf("unexpected calls: inspect %d, list %d, send %d", p.inspects, p.listings, len(p.sent))
	}
	if status, err := h.e.Poll(ctx, h.path, &PollListing{}); err != nil || status != StatusSent {
		t.Fatalf("verified failure: status %q, error %v", status, err)
	}
	if p.inspects != 5 || len(p.sent) != 1 {
		t.Fatalf("verified failure used inspect %d, send %d", p.inspects, len(p.sent))
	}
}

func TestPollBatchesFallbackForMalformedRecords(t *testing.T) {
	h := newHarness(t)
	h.e.storedFn = nil
	p := &pollingPaseo{fakePaseo: h.paseo, items: []*Object{
		Obj("id", "agent-id", "status", "running"),
		Obj("id", "another-agent", "status", "initializing"),
	}}
	h.e.Paseo = p
	dir := filepath.Join(h.home, "agents", "project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	batch := &PollListing{}
	for _, id := range []string{"agent-id", "another-agent"} {
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"lastStatus":`), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(h.root, id+".json")
		state := h.state.Clone()
		state.Set("agent_id", id)
		if err := atomicWrite(path, state); err != nil {
			t.Fatal(err)
		}
		if status, err := h.e.Poll(context.Background(), path, batch); err != nil || status != StatusWaitingFailure {
			t.Fatalf("%s: status %q, error %v", id, status, err)
		}
	}
	if p.listings != 1 || p.inspects != 0 || len(p.sent) != 0 {
		t.Fatalf("batch used inspect %d, list %d, send %d", p.inspects, p.listings, len(p.sent))
	}
}

func TestReadAgentRecordRetriesDecodeOnce(t *testing.T) {
	for _, recoverOnRetry := range []bool{false, true} {
		calls := 0
		record, err := readAgentRecord("synthetic", func(string) ([]byte, error) {
			calls++
			if recoverOnRetry && calls == 2 {
				return []byte(`{"id":"synthetic","lastStatus":"running"}`), nil
			}
			return []byte(`{"id":"synthetic",`), nil
		})
		if calls != 2 || (err == nil) != recoverOnRetry {
			t.Fatalf("recover=%v: reads %d, error %v", recoverOnRetry, calls, err)
		}
		if recoverOnRetry && StoredStatus(record) != "running" {
			t.Fatalf("unexpected recovered record: %v", record)
		}
	}
}

func TestCheckRetryStartsShortAndCapsAtNormalInterval(t *testing.T) {
	var retry CheckRetry
	now := time.Unix(2000, 0)
	for i, delay := range []time.Duration{20, 40, 60, 60} {
		if first := retry.Failed(now, time.Minute); first != (i == 0) {
			t.Fatalf("failure %d marked first=%v", i, first)
		}
		if got := retry.At.Sub(now); got != delay*time.Second {
			t.Fatalf("failure %d scheduled %s", i, got)
		}
		now = retry.At
	}
	retry = CheckRetry{}
	if !retry.Failed(now, 10*time.Second) || retry.At.Sub(now) != 10*time.Second {
		t.Fatalf("ten-second cycle: retry %+v", retry)
	}
}
