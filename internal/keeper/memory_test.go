//go:build unix

package keeper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// readBound is what a read may take here. The design target is 50 ms on
// real data; the bound is loose because the race detector and a loaded
// machine slow tests down, and the actual times are logged.
const readBound = 500 * time.Millisecond

func synthID(prefix string, i int) string {
	return fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, i)
}

type getter struct {
	t *testing.T
	m mux
}

func newGetter(h *harness) getter {
	m := mux{http.NewServeMux()}
	h.k.Register(m)
	return getter{h.t, m}
}

// get answers path, its status, its body and how long it took.
func (g getter) get(path string) (int, map[string]any, time.Duration) {
	g.t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	g.m.ServeHTTP(rec, req)
	took := time.Since(start)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		g.t.Fatalf("%s: %v", path, err)
	}
	return rec.Code, out, took
}

// TestReadsServeFromMemory: 200 agents (100 with a policy, 100 only listed
// by Paseo) and slow external commands; no read runs a command, and reads
// stay fast.
func TestReadsServeFromMemory(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	var first string
	for i := 0; i < 200; i++ {
		id, session := synthID("00000000", i), synthID("22222222", i)
		h.agent(id, session)
		h.realTurn(session, t0, small)
		if i%2 == 0 {
			h.set(id, PolicyWarm)
			if first == "" {
				first = id
			}
		} else {
			h.paseo.list = append(h.paseo.list, paseo.Obj("id", id, "status", "idle", "name", "listed agent"))
		}
	}
	h.paseo.delay = 300 * time.Millisecond
	start := time.Now()
	h.k.prime(ctx) // what Run does before serving
	t.Logf("first observation of 100 registrations: %v", time.Since(start))
	h.k.RefreshListing(ctx)
	g := newGetter(h)
	calls := h.paseo.callCount()

	code, out, took := g.get("/v1/agents")
	agents, _ := out["agents"].([]any)
	t.Logf("GET /v1/agents: %d agents in %v", len(agents), took)
	if code != 200 || len(agents) != 100 || took > readBound {
		t.Fatalf("list: %d, %d agents, %v", code, len(agents), took)
	}
	if a := agents[0].(map[string]any); a["observed_at"] == nil {
		t.Fatalf("no observed_at: %v", a)
	}

	code, out, took = g.get("/v1/agents?all=1")
	agents, _ = out["agents"].([]any)
	t.Logf("GET /v1/agents?all=1: %d agents in %v", len(agents), took)
	if code != 200 || len(agents) != 200 || took > readBound || out["listed_at"] == nil {
		t.Fatalf("all: %d, %d agents, %v, listed_at %v", code, len(agents), took, out["listed_at"])
	}

	code, out, took = g.get("/v1/agents/" + first + "?home=" + h.home)
	t.Logf("GET /v1/agents/{id}: %v", took)
	agent, _ := out["agent"].(map[string]any)
	if code != 200 || took > readBound || agent["observed_at"] == nil {
		t.Fatalf("one: %d %v %v", code, took, out)
	}

	code, out, took = g.get("/v1/agents?policy=warm")
	agents, _ = out["agents"].([]any)
	if code != 200 || len(agents) != 100 || took > readBound {
		t.Fatalf("policy=warm: %d, %d agents, %v", code, len(agents), took)
	}
	if n := h.paseo.callCount(); n != calls {
		t.Fatalf("reads ran %d external commands", n-calls)
	}
}

// TestAllServesTheLastListing: all=1 adds what the last paseo ls said and
// never runs paseo ls itself.
func TestAllServesTheLastListing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.agent(agentB, sessionB)
	h.paseo.list = []*paseo.Object{paseo.Obj("id", agentA, "status", "idle")}
	if list, _ := h.k.Agents(ctx, "", "", true); len(list) != 0 || h.k.ListedAt("") != nil {
		t.Fatalf("before any listing: %d agents", len(list))
	}
	h.k.RefreshListing(ctx)
	h.paseo.list = append(h.paseo.list, paseo.Obj("id", agentB, "status", "idle"))
	calls := h.paseo.callCount()
	list, _ := h.k.Agents(ctx, "", "", true)
	if len(list) != 1 || list[0].ID != agentA || h.k.ListedAt("") == nil {
		t.Fatalf("listing: %+v", list)
	}
	if h.paseo.callCount() != calls {
		t.Fatal("all=1 ran paseo ls")
	}
	h.k.RefreshListing(ctx)
	if list, _ := h.k.Agents(ctx, "", "", true); len(list) != 2 {
		t.Fatalf("after the next listing: %d agents", len(list))
	}
}

// TestRegistrationGoneIsDropped: a removed registration leaves the list at
// the next round.
func TestRegistrationGoneIsDropped(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.set(agentA, PolicyWarm)
	if list, _ := h.k.Agents(ctx, "", "", false); len(list) != 1 {
		t.Fatalf("list %d", len(list))
	}
	if err := os.Remove(h.path(agentA)); err != nil {
		t.Fatal(err)
	}
	h.k.Round(ctx, true)
	if list, _ := h.k.Agents(ctx, "", "", false); len(list) != 0 {
		t.Fatalf("after removal: %+v", list)
	}
	if v, err := h.k.Agent(ctx, agentA, h.home); err != nil || v.Cache != nil {
		t.Fatalf("agent without a file: %v %+v", err, v.Cache)
	}
}

// TestHungTranscriptStallsOnlyItsAgent: one agent's transcript is a FIFO
// nobody writes, so reading it hangs. Rounds go on for the others, and
// reads of the hung agent answer from its last observation.
func TestHungTranscriptStallsOnlyItsAgent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.agent(agentB, sessionB)
	h.realTurn(sessionA, t0, small)
	h.realTurn(sessionB, t0, small)
	h.set(agentA, PolicyWarm)
	h.set(agentB, PolicyWarm)
	if list, _ := h.k.Agents(ctx, "", "", false); len(list) != 2 {
		t.Fatalf("list %d", len(list))
	}
	fifo := h.transcriptPath(sessionB)
	os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Cleanup(func() { unhang(t, fifo) })

	h.now = t0 + 600
	h.realTurn(sessionA, h.now, small)
	h.k.round(ctx, true, false, false) // returns at once
	deadline := time.Now().Add(5 * time.Second)
	for {
		e, _ := h.k.cached(h.home, agentA)
		if e != nil && e.o.act.RealAt == h.now {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent A was not observed while B hangs")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Another round while B still hangs queues B behind itself.
	h.k.round(ctx, true, false, false)
	start := time.Now()
	v, err := h.k.Agent(ctx, agentB, h.home)
	if err != nil || time.Since(start) > readBound || v.ObservedAt == nil {
		t.Fatalf("read of the hung agent: %v after %v", err, time.Since(start))
	}
	list, _ := h.k.Agents(ctx, "", "", false)
	if len(list) != 2 || time.Since(start) > readBound {
		t.Fatalf("list while hung: %d after %v", len(list), time.Since(start))
	}
}

// unhang releases readers blocked on the FIFO: a writer opens it (pairing
// with a blocked reader), a regular file replaces the path so later opens
// do not block, and closing the writer gives the reader EOF.
func unhang(t *testing.T, fifo string) {
	tmp := fifo + ".tmp"
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Log(err)
		return
	}
	opened := make(chan *os.File, 1)
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err == nil {
			opened <- w
		}
	}()
	var w *os.File
	select {
	case w = <-opened:
	case <-time.After(2 * time.Second):
	}
	if err := os.Rename(tmp, fifo); err != nil {
		t.Log(err)
	}
	if w != nil {
		w.Close()
	}
	time.Sleep(50 * time.Millisecond)
}

// TestSlowInspectDoesNotDelayReads: a policy change runs paseo inspect
// once; reads meanwhile answer from memory.
func TestSlowInspectDoesNotDelayReads(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.agent(agentB, sessionB)
	h.set(agentA, PolicyWarm)
	if list, _ := h.k.Agents(ctx, "", "", false); len(list) != 1 {
		t.Fatalf("list %d", len(list))
	}
	h.paseo.delay = time.Second
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.k.SetPolicy(ctx, agentB, h.home, PolicyCompact, 0, "cli")
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if _, err := h.k.Agent(ctx, agentA, h.home); err != nil || time.Since(start) > readBound {
		t.Fatalf("read during a slow inspect: %v after %v", err, time.Since(start))
	}
	<-done
	h.paseo.mu.Lock()
	inspects := h.paseo.calls["inspect"]
	h.paseo.mu.Unlock()
	if inspects != 2 { // one per SetPolicy: A's and B's
		t.Fatalf("inspects = %d, want 2", inspects)
	}
	if list, _ := h.k.Agents(ctx, "", "compact", false); len(list) != 1 || list[0].ID != agentB {
		t.Fatalf("after the policy change: %+v", list)
	}
}
