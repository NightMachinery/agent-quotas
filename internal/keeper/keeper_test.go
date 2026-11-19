package keeper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

const (
	big   = 200000
	small = 50000
	ttl   = 3600.0
	lead  = 300.0
)

func TestCompactPolicyCompactsBigContextBeforeExpiry(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, big)
	h.set(agentA, PolicyCompact)

	v := h.view(agentA)
	if v.Cache.Phase != PhaseCompact || v.NextAction.Kind != ActCompact || *v.NextAction.At != t0+ttl-lead {
		t.Fatalf("plan = %+v phase %s", v.NextAction, v.Cache.Phase)
	}
	if v.LastActivitySource != SourceTranscript || *v.LastActivityAt != t0 || !v.Big || *v.ContextTokens != big {
		t.Fatalf("activity/context = %+v", v)
	}
	h.now = t0 + ttl - lead - 1
	h.step(agentA)
	h.wantSends(0)

	h.now = t0 + ttl - lead + 1
	h.step(agentA)
	s := h.wantSends(1)
	if s[0].text != paseo.CompactText || s[0].behavior != "steer" {
		t.Fatalf("sent %+v", s[0])
	}
	comp := h.cache(agentA).Compaction
	if comp.State != StateSent || comp.Reason != "policy" || *comp.TokensBefore != big {
		t.Fatalf("compaction %+v", comp)
	}
}

func TestCompactPolicyLeavesSmallContextCold(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.set(agentA, PolicyCompact)
	h.now = t0 + ttl - lead + 1
	h.step(agentA)
	h.wantSends(0)
	if v := h.view(agentA); v.NextAction.Kind != ActNone || v.Big {
		t.Fatalf("plan %+v", v.NextAction)
	}
}

func TestWarmPingsUntilCeilingThenDormant(t *testing.T) {
	h := newHarness(t)
	h.cfg.Keeper.WarmHours = 1.5
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.set(agentA, PolicyWarm)
	v := h.view(agentA)
	if v.Cache.Phase != PhaseWarming || *v.WarmUntil != t0+5400 || v.NextAction.Kind != ActPing {
		t.Fatalf("view %+v %+v", v.Cache, v.NextAction)
	}

	h.now = t0 + ttl - lead + 1
	ping1 := h.now
	h.step(agentA)
	s := h.wantSends(1)
	if !contains(s[0].text, paseo.KeeperMarker) || s[0].text[:len(paseo.KeeperMarker)] != paseo.KeeperMarker {
		t.Fatalf("ping text %q", s[0].text)
	}
	// The ping's reply lands in the transcript: cache activity, not real.
	h.appendT(sessionA, userLine(ping1+1, s[0].text), assistantLine(ping1+5, small))
	v = h.view(agentA)
	if *v.LastActivityAt != t0 || v.PingsSinceActivity != 1 || *v.CacheActivityAt != ping1+5 {
		t.Fatalf("after ping: real %v cache %v pings %d", *v.LastActivityAt, *v.CacheActivityAt, v.PingsSinceActivity)
	}
	if v.Cache.LastPing == nil || !v.Cache.LastPing.OK {
		t.Fatalf("last ping %+v", v.Cache.LastPing)
	}
	// The next ping would come after the 1.5 h ceiling: nothing more.
	if v.NextAction.Kind != ActNone {
		t.Fatalf("next %+v", v.NextAction)
	}
	h.now = t0 + 5400 + 1
	if v := h.view(agentA); v.Cache.Phase != PhaseDormant || v.Cache.Policy != PolicyWarm {
		t.Fatalf("phase %s policy %s", v.Cache.Phase, v.Cache.Policy)
	}
	h.now = ping1 + 5 + ttl - lead + 1
	h.step(agentA)
	h.wantSends(1)
}

func TestWarmBigCompactsOnceThenPings(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, big)
	h.set(agentA, PolicyWarm)
	h.now = t0 + ttl - lead + 1
	h.step(agentA)
	if s := h.wantSends(1); s[0].text != paseo.CompactText {
		t.Fatalf("first send %q", s[0].text)
	}
	// Paseo reports the compaction done; the transcript shows the boundary.
	h.paseo.logs[agentA] = []*paseo.Object{paseo.Obj("type", "user_message", "text", "/compact"),
		paseo.Obj("type", "compaction", "status", "completed")}
	h.appendT(sessionA, userLine(h.now+1, "<command-name>/compact</command-name>"), compactBoundaryLine(h.now+30))
	h.now += 40
	h.step(agentA)
	comp := h.cache(agentA).Compaction
	if comp.State != StateCompleted {
		t.Fatalf("compaction %+v", comp)
	}
	v := h.view(agentA)
	if *v.LastActivityAt != t0 || v.NextAction.Kind != ActPing {
		t.Fatalf("after compaction: real %v next %+v", *v.LastActivityAt, v.NextAction)
	}
	h.now = *v.NextAction.At + 1
	h.step(agentA)
	if s := h.wantSends(2); !contains(s[1].text, paseo.KeeperMarker) {
		t.Fatalf("second send %q", s[1].text)
	}
}

func TestRealTurnRearmsWarming(t *testing.T) {
	h := newHarness(t)
	h.cfg.Keeper.WarmHours = 1
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.set(agentA, PolicyWarm)
	h.now = t0 + 2*3600
	if v := h.view(agentA); v.Cache.Phase != PhaseDormant {
		t.Fatalf("phase %s", v.Cache.Phase)
	}
	h.realTurn(sessionA, h.now, small)
	v := h.view(agentA)
	if v.Cache.Phase != PhaseWarming || *v.WarmUntil != h.now+3600 || v.NextAction.Kind != ActPing {
		t.Fatalf("re-armed: %s %v %+v", v.Cache.Phase, *v.WarmUntil, v.NextAction)
	}
}

func TestStandbyIsStickyAndCompacts(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, big)
	h.set(agentA, PolicyWarm)
	v, err := h.k.Standby(context.Background(), agentA, h.home, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if v.Cache.Policy != PolicyCompact || v.Cache.Phase != PhaseStandby || v.Cache.SetBy != "agent" || v.Cache.StandbyAt == nil {
		t.Fatalf("standby %+v", v.Cache)
	}
	h.realTurn(sessionA, t0+100, big)
	if v := h.view(agentA); v.Cache.Phase != PhaseStandby {
		t.Fatalf("a real turn left standby: %s", v.Cache.Phase)
	}
	h.now = t0 + 100 + ttl - lead + 1
	h.step(agentA)
	if s := h.wantSends(1); s[0].text != paseo.CompactText {
		t.Fatalf("sent %q", s[0].text)
	}
	if c := h.cache(agentA).Compaction; c.Reason != "standby" {
		t.Fatalf("reason %q", c.Reason)
	}
}

func TestMissedWindowIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.set(agentA, PolicyWarm)
	h.now = t0 + ttl + 10 // slept past the expiry
	h.step(agentA)
	h.wantSends(0)
	if v := h.view(agentA); v.NextAction.Kind != ActNone || !contains(v.NextAction.Reason, "missed") {
		t.Fatalf("next %+v", v.NextAction)
	}
}

func TestSendableGating(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness)
		want  string
	}{
		{"running", func(h *harness) { h.setInspect(agentA, "Status", "running") }, "running"},
		{"pending permission", func(h *harness) {
			h.setInspect(agentA, "PendingPermissions", []any{paseo.Obj("id", "p1", "tool", "Bash")})
		}, "pending permission"},
		{"closed (ping)", func(h *harness) { h.setInspect(agentA, "Status", "closed") }, "closed"},
		{"blocked seat", func(h *harness) { h.seats.seat.Verdict = model.Blocked }, "blocked"},
		{"unclaimed quota failure", func(h *harness) {
			h.appendT(sessionA, userLine(t0+10, "synthetic task"), limitLine(t0+11))
		}, "quota-failed"},
		{"recent message", func(h *harness) {
			h.record(agentA, sessionA, "idle", map[string]any{"lastUserMessageAt": iso(t0 + ttl - lead - 20)})
		}, "recently"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.agent(agentA, sessionA)
			h.realTurn(sessionA, t0, small)
			h.set(agentA, PolicyWarm)
			tc.setup(h)
			h.now = t0 + ttl - lead + 1
			h.step(agentA)
			h.wantSends(0)
			v := h.view(agentA)
			if v.Sendable.OK || !contains(v.Sendable.Reason, tc.want) {
				t.Fatalf("sendable %+v, want %q", v.Sendable, tc.want)
			}
		})
	}
}

func TestClaimedFailureDoesNotBlock(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.appendT(sessionA, userLine(t0+10, "synthetic task"), limitLine(t0+11))
	h.set(agentA, PolicyWarm)
	v := h.view(agentA)
	if v.Sendable.OK {
		t.Fatal("unclaimed failure should block")
	}
	// Auto-continue claims it; the keeper may send again.
	o, err := h.k.observe(context.Background(), agentA, h.home, true)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := paseo.ReadObject(h.path(agentA))
	state.Set("claimed_failure", o.failure)
	if err := paseo.AtomicWrite(h.path(agentA), state); err != nil {
		t.Fatal(err)
	}
	if v := h.view(agentA); !v.Sendable.OK {
		t.Fatalf("claimed failure still blocks: %+v", v.Sendable)
	}
}

func TestClosedAgentIsSendableForCompaction(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, big)
	h.set(agentA, PolicyCompact)
	h.setInspect(agentA, "Status", "closed")
	h.now = t0 + ttl - lead + 1
	h.step(agentA)
	if s := h.wantSends(1); s[0].text != paseo.CompactText {
		t.Fatalf("sent %q", s[0].text)
	}
}

func TestQuietAfterAnotherLockHolder(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.set(agentA, PolicyWarm)
	h.now = t0 + ttl - lead + 1
	unlock, err := paseo.LockFile(paseo.LockPath(h.path(agentA)))
	if err != nil {
		t.Fatal(err)
	}
	h.step(agentA)
	unlock()
	h.wantSends(0)
	h.now += 30 // still within quiet_seconds (60)
	h.step(agentA)
	h.wantSends(0)
	if v := h.view(agentA); !contains(v.Sendable.Reason, "lock held") {
		t.Fatalf("sendable %+v", v.Sendable)
	}
	h.now += 31
	h.step(agentA)
	h.wantSends(1)
}

func TestAgentsWithoutPolicyAreLeftAlone(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, big)
	h.now = t0 + ttl - lead + 1
	h.k.Round(context.Background(), true)
	h.wantSends(0)
	if _, err := os.Stat(h.path(agentA)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file appeared: %v", err)
	}
}

func TestManualCompactionStateMachine(t *testing.T) {
	ctx := context.Background()
	t.Run("completed", func(t *testing.T) {
		h := newHarness(t)
		h.agent(agentA, sessionA)
		h.realTurn(sessionA, t0, small) // small: manual ignores big
		res, err := h.k.Compact(ctx, agentA, h.home)
		if err != nil {
			t.Fatal(err)
		}
		if res.State != StateSent || res.CompactionID != "c-000001" {
			t.Fatalf("result %+v", res)
		}
		h.wantSends(1)
		// Still running: nothing decided.
		h.setInspect(agentA, "Status", "running")
		h.paseo.logs[agentA] = []*paseo.Object{paseo.Obj("type", "compaction", "status", "loading")}
		h.now += 20
		h.step(agentA)
		if c := h.cache(agentA).Compaction; c.State != StateSent {
			t.Fatalf("state %s", c.State)
		}
		h.setInspect(agentA, "Status", "idle")
		h.paseo.logs[agentA] = append(h.paseo.logs[agentA], paseo.Obj("type", "compaction", "status", "completed"))
		h.step(agentA)
		c := h.cache(agentA).Compaction
		if c.State != StateCompleted || c.Reason != "manual" || c.FinishedAt != h.now {
			t.Fatalf("compaction %+v", c)
		}
		// tokens_after arrives with the next real model call.
		h.appendT(sessionA, compactBoundaryLine(h.now-5))
		h.realTurn(sessionA, h.now+100, 31000)
		h.now += 200
		h.step(agentA)
		if c := h.cache(agentA).Compaction; c.TokensAfter == nil || *c.TokensAfter != 31000 {
			t.Fatalf("tokens_after %+v", c.TokensAfter)
		}
	})
	t.Run("failed send", func(t *testing.T) {
		h := newHarness(t)
		h.agent(agentA, sessionA)
		h.paseo.sendErr = errors.New("External command failed")
		res, err := h.k.Compact(ctx, agentA, h.home)
		if err != nil {
			t.Fatal(err)
		}
		c := h.cache(agentA).Compaction
		if res.State != StateFailed || c.State != StateFailed || c.Error != "External command failed" {
			t.Fatalf("result %+v compaction %+v", res, c)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h := newHarness(t)
		h.agent(agentA, sessionA)
		if _, err := h.k.Compact(ctx, agentA, h.home); err != nil {
			t.Fatal(err)
		}
		h.setInspect(agentA, "Status", "running")
		h.now += 1201
		h.step(agentA)
		if c := h.cache(agentA).Compaction; c.State != StateFailed || c.Error != "timeout" {
			t.Fatalf("compaction %+v", c)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		h := newHarness(t)
		h.agent(agentA, sessionA)
		if _, err := h.k.Compact(ctx, agentA, h.home); err != nil {
			t.Fatal(err)
		}
		h.paseo.logs[agentA] = []*paseo.Object{paseo.Obj("type", "compaction", "status", "loading"),
			paseo.Obj("type", "assistant_message", "text", "Compaction canceled.")}
		h.now += 15
		h.step(agentA)
		if c := h.cache(agentA).Compaction; c.State != StateCanceled {
			t.Fatalf("compaction %+v", c)
		}
	})
	t.Run("turn ended without completing", func(t *testing.T) {
		h := newHarness(t)
		h.agent(agentA, sessionA)
		if _, err := h.k.Compact(ctx, agentA, h.home); err != nil {
			t.Fatal(err)
		}
		h.paseo.logs[agentA] = []*paseo.Object{paseo.Obj("type", "user_message", "text", "/compact")}
		h.now += 5
		h.step(agentA)
		if c := h.cache(agentA).Compaction; c.State != StateSent {
			t.Fatalf("decided too early: %+v", c)
		}
		h.now += 10
		h.step(agentA)
		if c := h.cache(agentA).Compaction; c.State != StateCanceled || !contains(c.Error, "without completing") {
			t.Fatalf("compaction %+v", c)
		}
	})
	t.Run("not idle", func(t *testing.T) {
		h := newHarness(t)
		h.agent(agentA, sessionA)
		h.setInspect(agentA, "Status", "running")
		res, err := h.k.Compact(ctx, agentA, h.home)
		if err != nil || res.State != StateQueued {
			t.Fatalf("%+v %v", res, err)
		}
		h.now += 600
		h.step(agentA)
		h.wantSends(0)
		if c := h.cache(agentA).Compaction; c.State != StateQueued {
			t.Fatalf("state %s", c.State)
		}
		h.now += 601
		h.step(agentA)
		if c := h.cache(agentA).Compaction; c.State != StateFailed || c.Error != "not idle" {
			t.Fatalf("compaction %+v", c)
		}
	})
	t.Run("replaced", func(t *testing.T) {
		h := newHarness(t)
		h.agent(agentA, sessionA)
		h.setInspect(agentA, "Status", "running")
		first, _ := h.k.Compact(ctx, agentA, h.home)
		second, err := h.k.Compact(ctx, agentA, h.home)
		if err != nil || second.CompactionID == first.CompactionID || second.State != StateQueued {
			t.Fatalf("first %+v second %+v %v", first, second, err)
		}
		// A request while one is sent returns that one.
		h.setInspect(agentA, "Status", "idle")
		h.step(agentA)
		third, _ := h.k.Compact(ctx, agentA, h.home)
		if third.CompactionID != second.CompactionID || third.State != StateSent {
			t.Fatalf("third %+v", third)
		}
		h.wantSends(1)
	})
	t.Run("archived", func(t *testing.T) {
		h := newHarness(t)
		h.agent(agentA, sessionA)
		if _, err := h.k.Compact(ctx, agentA, h.home); err != nil {
			t.Fatal(err)
		}
		h.setInspect(agentA, "Archived", true)
		h.now += 5
		h.step(agentA)
		if c := h.cache(agentA).Compaction; c.State != StateFailed || c.Error != "archived" {
			t.Fatalf("compaction %+v", c)
		}
	})
}

func TestLongPoll(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.setInspect(agentA, "Status", "running")
	res, err := h.k.Compact(ctx, agentA, h.home)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	v, err := h.k.WaitCompaction(ctx, agentA, h.home, res.CompactionID, 50*time.Millisecond)
	if err != nil || v.Cache.Compaction.State != StateQueued || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("timeout answer: %v %+v", err, v.Cache.Compaction)
	}
	done := make(chan View, 1)
	go func() {
		v, _ := h.k.WaitCompaction(ctx, agentA, h.home, res.CompactionID, 10*time.Second)
		done <- v
	}()
	time.Sleep(50 * time.Millisecond)
	h.k.finish(h.path(agentA), res.CompactionID, StateCanceled, "test")
	select {
	case v := <-done:
		if v.Cache.Compaction.State != StateCanceled {
			t.Fatalf("state %s", v.Cache.Compaction.State)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long-poll did not answer on the change")
	}
	// An unknown compaction id answers at once.
	start = time.Now()
	if _, err := h.k.WaitCompaction(ctx, agentA, h.home, "c-other", 10*time.Second); err != nil || time.Since(start) > time.Second {
		t.Fatalf("unknown id: %v after %v", err, time.Since(start))
	}
}

func TestMarkerEnforcement(t *testing.T) {
	if got := PingText("hello\nthere"); got != paseo.KeeperMarker+" hello there" {
		t.Fatalf("PingText = %q", got)
	}
	if got := PingText(paseo.KeeperMarker + " ok"); got != paseo.KeeperMarker+" ok" {
		t.Fatalf("PingText kept marker = %q", got)
	}
	h := newHarness(t)
	h.cfg.Keeper.PingText = "custom keepalive"
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.set(agentA, PolicyWarm)
	h.now = t0 + ttl - lead + 1
	h.step(agentA)
	if s := h.wantSends(1); s[0].text != paseo.KeeperMarker+" custom keepalive" {
		t.Fatalf("sent %q", s[0].text)
	}
}

func TestOverageTTL(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, small)
	h.set(agentA, PolicyWarm)
	if v := h.view(agentA); v.TTLSeconds != 3600 || v.TTLReason != "config" {
		t.Fatalf("ttl %d %s", v.TTLSeconds, v.TTLReason)
	}
	h.seats.seat = Seat{Account: "claude:default", Verdict: model.Blocked, Overage: true}
	v := h.view(agentA)
	if v.TTLSeconds != 300 || v.TTLReason != "overage" || *v.CacheExpiresAt != t0+300 {
		t.Fatalf("overage ttl %d %s %v", v.TTLSeconds, v.TTLReason, *v.CacheExpiresAt)
	}
}

func TestDefaultPolicyFilter(t *testing.T) {
	h := newHarness(t)
	h.cfg.Keeper.DefaultPolicy = PolicyWarm
	h.now = t0 + 25*3600
	h.agent(agentA, sessionA) // recent
	h.realTurn(sessionA, h.now-3600, small)
	h.agent(agentB, sessionB) // old
	h.realTurn(sessionB, t0, small)
	h.agent(agentC, sessionC) // archived
	h.realTurn(sessionC, h.now-60, small)
	h.record(agentC, sessionC, "idle", map[string]any{"archivedAt": iso(h.now)})
	h.agent(agentD, sessionD) // a subagent, by label
	h.realTurn(sessionD, h.now-60, small)
	h.record(agentD, sessionD, "idle", map[string]any{"labels": paseo.Obj(parentLabel, agentA)})
	h.agent(agentE, sessionE) // a subagent, by parent id
	h.realTurn(sessionE, h.now-60, small)
	h.record(agentE, sessionE, "idle", map[string]any{"parentAgentId": agentA})
	for _, id := range []string{agentA, agentB, agentC, agentD, agentE} {
		h.paseo.list = append(h.paseo.list, paseo.Obj("id", id, "status", "idle"))
	}
	h.k.RefreshListing(context.Background())
	if c := h.cache(agentA); c == nil || c.Policy != PolicyWarm || c.SetBy != "default" {
		t.Fatalf("recent agent: %+v", c)
	}
	for _, id := range []string{agentB, agentC, agentD, agentE} {
		if _, err := os.Stat(h.path(id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s got a file", id)
		}
	}
	// Decided policies are not overwritten.
	h.set(agentA, PolicyOff)
	h.k.RefreshListing(context.Background())
	if c := h.cache(agentA); c.Policy != PolicyOff {
		t.Fatalf("default overwrote a decision: %+v", c)
	}
}

func TestDisabledKeeperReadsButRefusesActs(t *testing.T) {
	h := newHarness(t)
	off := false
	h.cfg.Keeper.Enabled = &off
	h.agent(agentA, sessionA)
	if _, err := h.k.SetPolicy(context.Background(), agentA, h.home, PolicyWarm, 0, "cli"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("SetPolicy err = %v", err)
	}
	if _, err := h.k.Compact(context.Background(), agentA, h.home); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Compact err = %v", err)
	}
	if v := h.view(agentA); v.Cache != nil || v.LockPath != nil || v.NextAction.Kind != ActNone {
		t.Fatalf("view %+v", v)
	}
	list, err := h.k.Agents(context.Background(), "", "", true)
	if err != nil || len(list) != 0 {
		t.Fatalf("list %v %v (paseo lists nothing)", list, err)
	}
	h.k.Step(context.Background(), filepath.Join(h.root, "nothing.json"))
}

func TestPolicyOnlyFileAndAutoContinueCoexist(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.set(agentA, PolicyWarm)
	state, err := paseo.ReadObject(h.path(agentA))
	if err != nil {
		t.Fatal(err)
	}
	if enabled, _ := state.Value("enabled").(bool); enabled {
		t.Fatal("policy-only file enabled auto-continue")
	}
	if b, ok := state.Object("binding"); !ok || b.Value("agent") != "claude" {
		t.Fatalf("binding %v", state.Value("binding"))
	}
	// Setting off keeps the file (and auto-continue's fields).
	h.set(agentA, PolicyOff)
	if c := h.cache(agentA); c.Policy != PolicyOff {
		t.Fatalf("policy %s", c.Policy)
	}
	list, _ := h.k.Agents(context.Background(), "", "any", false)
	if len(list) != 0 {
		t.Fatalf("policy=any listed %d", len(list))
	}
	list, _ = h.k.Agents(context.Background(), "", "off", false)
	if len(list) != 1 || list[0].LockPath == nil {
		t.Fatalf("policy=off list %+v", list)
	}
}

func TestUnknownAgent(t *testing.T) {
	h := newHarness(t)
	if _, err := h.k.Agent(context.Background(), agentB, h.home); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("Agent err = %v", err)
	}
	if _, err := h.k.SetPolicy(context.Background(), agentB, h.home, PolicyWarm, 0, "cli"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("SetPolicy err = %v", err)
	}
}

func TestMinContextTokens(t *testing.T) {
	dir := realTemp(t)
	p := filepath.Join(dir, "c.yaml")
	if n := MinContextTokens(p); n != defaultMinContext {
		t.Fatalf("absent file: %d", n)
	}
	os.WriteFile(p, []byte("other: 1\nmin_context_tokens: 64_000 # note\n"), 0o600)
	if n := MinContextTokens(p); n != 64000 {
		t.Fatalf("got %d", n)
	}
}

func TestDefaultAutoContinueFilter(t *testing.T) {
	h := newHarness(t)
	h.now = t0 + 25*3600
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, h.now-3600, small)
	h.set(agentA, PolicyWarm) // a cache-only file leaves auto-continue undecided
	h.agent(agentB, sessionB)
	h.realTurn(sessionB, t0, small)
	h.agent(agentC, sessionC)
	h.record(agentC, sessionC, "idle", map[string]any{"archivedAt": iso(h.now)})
	h.agent(agentD, sessionD)
	h.record(agentD, sessionD, "idle", map[string]any{"labels": paseo.Obj(parentLabel, agentA)})
	h.agent(agentE, sessionE)
	h.record(agentE, sessionE, "idle", map[string]any{"parentAgentId": agentA})
	for _, id := range []string{agentA, agentB, agentC, agentD, agentE} {
		h.paseo.list = append(h.paseo.list, paseo.Obj("id", id))
	}

	ctx := context.Background()
	h.k.RefreshListing(ctx)
	if ac := h.view(agentA).AutoContinue; ac == nil || ac.Enabled {
		t.Fatalf("default was already enabled: %+v", ac)
	}
	h.cfg.Paseo.DefaultAutoContinue = true
	off := false
	h.cfg.Paseo.Enabled = &off
	h.k.RefreshListing(ctx)
	if ac := h.view(agentA).AutoContinue; ac == nil || ac.Enabled {
		t.Fatalf("default ran while the Paseo engine was disabled: %+v", ac)
	}
	h.cfg.Paseo.Enabled = nil
	h.cfg.Keeper.Enabled = &off
	h.k.RefreshListing(ctx)
	if ac := h.view(agentA).AutoContinue; ac == nil || !ac.Enabled || ac.SetBy != "default" {
		t.Fatalf("recent main agent: %+v", ac)
	}
	if c := h.cache(agentA); c == nil || c.Policy != PolicyWarm {
		t.Fatalf("default changed cache policy: %+v", c)
	}
	for _, id := range []string{agentB, agentC, agentD, agentE} {
		if _, err := os.Stat(h.path(id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s got a file: %v", id, err)
		}
	}
	if _, err := h.eng.Off(ctx, agentA, h.home); err != nil {
		t.Fatal(err)
	}
	h.k.RefreshListing(ctx)
	h.k.refreshPath(h.path(agentA))
	if ac := h.view(agentA).AutoContinue; ac == nil || ac.Enabled || ac.SetBy != "cli" {
		t.Fatalf("default replaced explicit off: %+v", ac)
	}
}

func TestDefaultAutoContinuePreservesPriorDecisions(t *testing.T) {
	h := newHarness(t)
	h.cfg.Paseo.DefaultAutoContinue = true
	h.agent(agentA, sessionA)
	h.agent(agentB, sessionB)
	h.agent(agentC, sessionC)
	h.agent(agentD, sessionD)
	h.record(agentD, sessionD, "idle", map[string]any{"internal": true})
	ctx := context.Background()
	if _, err := h.eng.Off(ctx, agentA, h.home); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.setAutoContinue(ctx, agentB, h.home, false, "api"); err != nil {
		t.Fatal(err)
	}
	// Older cache-only and explicitly disabled records are indistinguishable.
	h.set(agentC, PolicyWarm)
	state, err := paseo.ReadObject(h.path(agentC))
	if err != nil {
		t.Fatal(err)
	}
	legacy := paseo.NewObject()
	for _, key := range state.Keys() {
		if key != "set_by" {
			legacy.Set(key, state.Value(key))
		}
	}
	if err := paseo.AtomicWrite(h.path(agentC), legacy); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{agentA, agentB, agentC, agentD} {
		h.paseo.list = append(h.paseo.list, paseo.Obj("id", id))
	}
	h.k.RefreshListing(ctx)
	for _, tc := range []struct{ id, source string }{{agentA, "cli"}, {agentB, "api"}, {agentC, ""}} {
		h.k.refreshPath(h.path(tc.id))
		if ac := h.view(tc.id).AutoContinue; ac == nil || ac.Enabled || ac.SetBy != tc.source {
			t.Fatalf("default changed %s: %+v", tc.id, ac)
		}
	}
	if _, err := os.Stat(h.path(agentD)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("internal agent got a file: %v", err)
	}
}
