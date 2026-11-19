package keeper

import (
	"context"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

func TestKeeperChecksFileFirstAndRetriesLiveTimeout(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.set(agentA, PolicyWarm)
	h.now = t0 + ttl - lead + 1
	h.record(agentA, sessionA, "running", nil)
	before := h.paseo.callCount()
	h.step(agentA)
	if got := h.paseo.callCount() - before; got != 0 {
		t.Fatalf("running file used %d calls", got)
	}
	h.wantSends(0)

	h.record(agentA, sessionA, "idle", nil)
	h.paseo.inspErr = paseo.ErrCommandUnavailable
	h.step(agentA)
	h.wantSends(0)
	if delay := h.k.retryAt(h.path(agentA)).Sub(h.k.now()); delay != 20*time.Second {
		t.Fatalf("retry delay %s", delay)
	}
	h.now += 10
	h.k.Round(context.Background(), false)
	if got := h.paseo.callCount() - before; got != 1 {
		t.Fatalf("checked during backoff: %d calls", got)
	}
	h.now += 10
	h.paseo.inspErr = nil
	h.setInspect(agentA, "PendingPermissions", []any{paseo.Obj("id", "p1")})
	h.k.Round(context.Background(), false)
	h.wantSends(0)
	if got := h.paseo.callCount() - before; got != 2 {
		t.Fatalf("retry used %d calls in total", got)
	}
	if !h.k.retryAt(h.path(agentA)).IsZero() {
		t.Fatal("successful inspect did not reset backoff")
	}
}

func TestKeeperBatchesCompactionStatusInOneListing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.agent(agentA, sessionA)
	h.agent(agentB, sessionB)
	for _, agent := range []string{agentA, agentB} {
		result, err := h.k.Compact(ctx, agent, h.home)
		if err != nil || result.State != StateSent {
			t.Fatalf("compact %s: %+v, %v", agent, result, err)
		}
		h.paseo.logs[agent] = []*paseo.Object{paseo.Obj("type", "compaction", "status", "completed")}
	}
	h.paseo.list = []*paseo.Object{
		paseo.Obj("id", agentA, "status", "idle"),
		paseo.Obj("id", agentB, "status", "closed"),
	}
	before := h.paseo.callCount()
	h.now += 10
	h.k.Round(ctx, false)
	if got := h.paseo.callCount() - before; got != 3 {
		t.Fatalf("batch used %d calls, want one list and two logs", got)
	}
	for _, agent := range []string{agentA, agentB} {
		if comp := h.cache(agent).Compaction; comp.State != StateCompleted {
			t.Fatalf("%s: compaction %+v", agent, comp)
		}
	}
	h.wantSends(2)
}
