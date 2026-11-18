package keeper

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// Loop timing.
const (
	tickEvery       = 10 * time.Second // compactions in flight are followed this often
	observeEvery    = 60 * time.Second // agents with a policy are observed this often
	defaultEvery    = 5 * time.Minute  // the default policy's agent listing
	defaultRecent   = 24 * time.Hour   // default policy: real activity this recent
	cancelGraceSecs = 10.0             // an idle agent this long after the send ended its turn
)

// cancelText is how a compaction turn that ended without compacting reads.
var cancelText = regexp.MustCompile(`(?i)compaction\s+cancel`)

// Run observes and acts until ctx ends. It does nothing while the keeper
// is disabled; the read API works either way.
func (k *Keeper) Run(ctx context.Context) {
	k.init()
	if !k.Enabled() {
		return
	}
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	var lastFull, lastDefault time.Time
	for {
		now := k.now()
		full := now.Sub(lastFull) >= observeEvery
		if full {
			lastFull = now
		}
		k.Round(ctx, full)
		if now.Sub(lastDefault) >= defaultEvery {
			lastDefault = now
			k.ApplyDefault(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case path := <-k.kick:
			k.Step(ctx, path)
		}
	}
}

// request asks the loop to look at one registration now.
func (k *Keeper) request(path string) {
	k.mu.Lock()
	k.init()
	ch := k.kick
	k.mu.Unlock()
	select {
	case ch <- path:
	default:
	}
}

// Round steps every registration with a policy (full) or with a compaction
// in flight (every round).
func (k *Keeper) Round(ctx context.Context, full bool) {
	regs, err := k.Engine.Registrations()
	if err != nil {
		k.Log.Printf("keeper: registrations: %v", err)
		return
	}
	for _, r := range regs {
		if ctx.Err() != nil {
			return
		}
		state, err := paseo.ReadObject(r.Path)
		if err != nil {
			continue
		}
		c, err := readCache(state)
		if err != nil || c == nil {
			continue
		}
		busy := c.Compaction.inFlight()
		if !busy && (!full || (c.Policy == PolicyOff || c.Policy == "") && !needsTokensAfter(c)) {
			continue
		}
		k.Step(ctx, r.Path)
	}
}

func needsTokensAfter(c *Cache) bool {
	return c.Compaction != nil && c.Compaction.State == StateCompleted && c.Compaction.TokensAfter == nil
}

// Step observes one registration and acts if an action is due.
func (k *Keeper) Step(ctx context.Context, path string) {
	if !k.Enabled() {
		return
	}
	state, err := paseo.ReadObject(path)
	if err != nil {
		return
	}
	agentID, _ := state.String("agent_id")
	home, _ := state.String("home")
	o, err := k.observe(ctx, agentID, home, false)
	if err != nil || o.cache == nil {
		return
	}
	k.bookkeep(o)
	if c := o.cache.Compaction; c != nil && c.State == StateQueued && o.archived() {
		k.finish(o.path, c.ID, StateFailed, "archived")
		return
	}
	if c := o.cache.Compaction; c != nil && c.State == StateSent {
		k.progress(ctx, o)
		return
	}
	v := k.view(o)
	now := k.nowS()
	if v.NextAction.Kind == ActNone || *v.NextAction.At > now {
		k.note(o.path, "next: "+v.NextAction.Kind+" ("+v.NextAction.Reason+")")
		return
	}
	// Due: look again with paseo inspect.
	if o, err = k.observe(ctx, agentID, home, true); err != nil {
		k.note(path, "inspect failed: "+err.Error())
		return
	}
	v = k.view(o)
	if v.NextAction.Kind == ActNone || *v.NextAction.At > now {
		return
	}
	if !v.Sendable.OK {
		if c := o.cache.Compaction; c != nil && c.State == StateQueued &&
			now-c.RequestedAt > float64(k.Config.Keeper.CompactionTimeoutSeconds) {
			k.finish(o.path, c.ID, StateFailed, "not idle")
			return
		}
		k.note(path, v.NextAction.Kind+" due, not sendable: "+v.Sendable.Reason)
		return
	}
	k.act(ctx, o, v.NextAction)
}

// note logs a line when an agent's situation changes.
func (k *Keeper) note(path, line string) {
	k.mu.Lock()
	k.init()
	same := k.lastSeen[path] == line
	k.lastSeen[path] = line
	k.mu.Unlock()
	if !same {
		k.Log.Printf("keeper %s: %s", agentOf(path), line)
	}
}

func agentOf(path string) string {
	state, err := paseo.ReadObject(path)
	if err != nil {
		return path
	}
	id, _ := state.String("agent_id")
	return id
}

// bookkeep records the real activity the keeper saw (the fallback when the
// transcript is gone) and the first context after a compaction.
func (k *Keeper) bookkeep(o *obs) {
	v := k.view(o)
	c := o.cache
	grow := v.realAt > c.ActivityAt+0.001
	after := needsTokensAfter(c) && o.act.HasContext && o.act.CacheAt > c.Compaction.FinishedAt
	if !grow && !after {
		return
	}
	_ = k.update(o.path, false, func(_ *paseo.Object, c *Cache) bool {
		changed := false
		if v.realAt > c.ActivityAt+0.001 {
			c.ActivityAt, changed = v.realAt, true
		}
		if after && needsTokensAfter(c) {
			n := o.act.ContextTokens
			c.Compaction.TokensAfter, changed = &n, true
		}
		return changed
	})
}

// update rewrites a registration's cache object under its lock. With wait,
// it waits for the lock; otherwise a held lock skips the update.
func (k *Keeper) update(path string, wait bool, fn func(state *paseo.Object, c *Cache) bool) error {
	var unlock func()
	if wait {
		u, err := paseo.LockFile(paseo.LockPath(path))
		if err != nil {
			return err
		}
		unlock = u
	} else {
		u, ok, err := paseo.TryLockFile(paseo.LockPath(path))
		if err != nil || !ok {
			return err
		}
		unlock = u
	}
	defer unlock()
	return k.rewrite(path, fn)
}

// rewrite is update's body, for callers that hold the lock.
func (k *Keeper) rewrite(path string, fn func(state *paseo.Object, c *Cache) bool) error {
	state, err := paseo.ReadObject(path)
	if err != nil {
		return err
	}
	c, err := readCache(state)
	if err != nil {
		return err
	}
	if c == nil {
		c = &Cache{Policy: PolicyOff}
	}
	if !fn(state, c) {
		return nil
	}
	if err := writeCache(state, c); err != nil {
		return err
	}
	if err := paseo.AtomicWrite(path, state); err != nil {
		return err
	}
	k.signal()
	return nil
}

// finish ends compaction id with a terminal state.
func (k *Keeper) finish(path, id, state, why string) {
	err := k.update(path, true, func(_ *paseo.Object, c *Cache) bool {
		comp := c.Compaction
		if comp == nil || comp.ID != id || comp.Terminal() {
			return false
		}
		comp.State, comp.Error, comp.FinishedAt = state, why, k.nowS()
		return true
	})
	if err != nil {
		k.Log.Printf("keeper %s: recording compaction %s %s: %v", agentOf(path), id, state, err)
		return
	}
	line := "compaction " + id + " " + state
	if why != "" {
		line += " (" + why + ")"
	}
	k.note(path, line)
}

// act sends one ping or /compact, rechecking everything under the lock.
func (k *Keeper) act(ctx context.Context, o *obs, a Action) {
	unlock, ok, err := paseo.TryLockFile(paseo.LockPath(o.path))
	if err != nil {
		k.note(o.path, "lock: "+err.Error())
		return
	}
	if !ok {
		k.mu.Lock()
		k.init()
		k.busy[o.path] = k.now()
		k.mu.Unlock()
		k.note(o.path, a.Kind+" due; lock held by another sender, staying quiet")
		return
	}
	defer unlock()
	fresh, err := k.observe(ctx, o.agentID, o.home, true)
	if err != nil || fresh.cache == nil {
		return
	}
	v := k.view(fresh)
	now := k.nowS()
	if v.NextAction.Kind != a.Kind || v.NextAction.At == nil || *v.NextAction.At > now || !v.Sendable.OK {
		k.note(o.path, a.Kind+" skipped on recheck: "+v.NextAction.Reason+" "+v.Sendable.Reason)
		return
	}
	behavior := k.Config.Keeper.SendBehavior
	switch a.Kind {
	case ActPing:
		if err := k.rewrite(o.path, func(_ *paseo.Object, c *Cache) bool {
			c.LastPing = &Ping{At: now, Error: "sending"}
			return true
		}); err != nil {
			return
		}
		sendErr := k.Paseo.Send(ctx, o.agentID, o.home, k.PingText(), behavior)
		_ = k.rewrite(o.path, func(_ *paseo.Object, c *Cache) bool {
			c.LastPing = &Ping{At: now, OK: sendErr == nil}
			if sendErr != nil {
				c.LastPing.Error = sendErr.Error()
				return true
			}
			if c.Pings.Count > 0 && c.Pings.Since > v.realAt-0.001 && c.Pings.Since < v.realAt+0.001 {
				c.Pings.Count++
			} else {
				c.Pings = Pings{Since: v.realAt, Count: 1}
			}
			return true
		})
		if sendErr != nil {
			k.note(o.path, "ping failed: "+sendErr.Error())
		} else {
			k.note(o.path, "ping sent")
		}
	case ActCompact:
		var id string
		if err := k.rewrite(o.path, func(_ *paseo.Object, c *Cache) bool {
			comp := c.Compaction
			if comp == nil || comp.State != StateQueued {
				reason := "policy"
				if v.Cache.Phase == PhaseStandby {
					reason = "standby"
				}
				comp = &Compaction{ID: k.newID(), Reason: reason, RequestedAt: now}
				c.Compaction = comp
			}
			comp.State, comp.SentAt = StateSent, now
			if v.ContextTokens != nil {
				n := *v.ContextTokens
				comp.TokensBefore = &n
			}
			id = comp.ID
			return true
		}); err != nil {
			return
		}
		sendErr := k.Paseo.Send(ctx, o.agentID, o.home, paseo.CompactText, behavior)
		if sendErr != nil {
			_ = k.rewrite(o.path, func(_ *paseo.Object, c *Cache) bool {
				if c.Compaction == nil || c.Compaction.ID != id {
					return false
				}
				c.Compaction.State, c.Compaction.Error, c.Compaction.FinishedAt = StateFailed, sendErr.Error(), k.nowS()
				return true
			})
			k.note(o.path, "compaction "+id+" failed: "+sendErr.Error())
			return
		}
		k.note(o.path, "compaction "+id+" sent")
	}
}

// progress follows a sent compaction to its end.
func (k *Keeper) progress(ctx context.Context, o *obs) {
	comp := o.cache.Compaction
	now := k.nowS()
	timeout := float64(k.Config.Keeper.CompactionTimeoutSeconds)
	fresh, err := k.observe(ctx, o.agentID, o.home, true)
	if err == nil {
		o = fresh
	}
	if o.archived() {
		k.finish(o.path, comp.ID, StateFailed, "archived")
		return
	}
	if err == nil {
		status := o.status()
		idle := status == "idle" || status == "closed"
		items, lerr := k.Paseo.Logs(ctx, o.agentID, o.home, model.Time(comp.SentAt-2))
		if lerr == nil {
			completed, canceled := scanTimeline(items)
			switch {
			case completed && idle:
				k.finish(o.path, comp.ID, StateCompleted, "")
				return
			case canceled && idle:
				k.finish(o.path, comp.ID, StateCanceled, "Compaction canceled.")
				return
			case idle && len(items) > 0 && now-comp.SentAt >= cancelGraceSecs:
				k.finish(o.path, comp.ID, StateCanceled, "the compaction turn ended without completing")
				return
			}
		}
	}
	if now-comp.SentAt > timeout {
		k.finish(o.path, comp.ID, StateFailed, "timeout")
	}
}

// scanTimeline looks for a completed compaction, or a cancellation, in
// Paseo timeline items.
func scanTimeline(items []*paseo.Object) (completed, canceled bool) {
	for _, it := range items {
		typ, _ := it.String("type")
		switch typ {
		case "compaction":
			if s, _ := it.String("status"); s == "completed" {
				completed = true
			}
		case "assistant_message", "error", "notification":
			for _, key := range []string{"text", "message"} {
				if s, _ := it.String(key); cancelText.MatchString(s) {
					canceled = true
				}
			}
		}
	}
	return completed, canceled
}

// ApplyDefault gives the default policy, once, to recently active agents
// whose policy was never decided.
func (k *Keeper) ApplyDefault(ctx context.Context) {
	policy := k.Config.Keeper.DefaultPolicy
	if policy == "" || policy == PolicyOff || !k.Enabled() {
		return
	}
	for _, h := range k.Config.Paseo.Homes {
		home := k.Engine.ResolveHome(h)
		list, err := k.Paseo.List(ctx, home)
		if err != nil {
			k.Log.Printf("keeper: paseo ls: %v", err)
			continue
		}
		for _, item := range list {
			raw, _ := item.String("id")
			id, ok := paseo.CanonicalID(raw)
			if !ok || ctx.Err() != nil {
				continue
			}
			o, err := k.observe(ctx, id, home, false)
			if err != nil || o.record == nil || o.archived() || o.record.Value("internal") == true {
				continue
			}
			if o.cache != nil && o.cache.SetAt > 0 {
				continue // decided before
			}
			v := k.view(o)
			if v.realAt <= 0 || k.nowS()-v.realAt > defaultRecent.Seconds() {
				continue
			}
			if _, err := k.setPolicy(ctx, id, home, policy, 0, "default", false); err != nil &&
				!errors.Is(err, ErrBadRequest) {
				k.Log.Printf("keeper %s: default policy: %v", id, err)
			}
		}
	}
}
