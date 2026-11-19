package keeper

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
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

// Run observes, and acts when the keeper is enabled, until ctx ends. It
// observes even while the keeper is disabled: the read API serves its
// observations.
func (k *Keeper) Run(ctx context.Context) {
	k.init()
	k.prime(ctx)
	go func() {
		ticker := time.NewTicker(defaultEvery)
		defer ticker.Stop()
		for {
			k.RefreshListing(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	var lastFull time.Time
	for {
		now := k.now()
		full := now.Sub(lastFull) >= observeEvery
		if full {
			lastFull = now
		}
		k.round(ctx, full, false, false)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case path := <-k.kick:
			k.stepAsync(ctx, path)
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

// stepAsync steps one registration on the worker pool.
func (k *Keeper) stepAsync(ctx context.Context, path string) {
	state, err := paseo.ReadObject(path)
	if err != nil {
		return
	}
	id, _ := state.String("agent_id")
	home, _ := state.String("home")
	if id == "" {
		return
	}
	k.dispatch(ctx, key(k.Engine.ResolveHome(home), id), func(ctx context.Context) { k.Step(ctx, path) }, nil)
}

// Round steps every registration with a policy (full) or with a compaction
// in flight (every round), and observes the others (full), then waits for
// that work.
func (k *Keeper) Round(ctx context.Context, full bool) { k.round(ctx, full, true, false) }

// round is Round. Each agent's work runs on the worker pool, at most one
// piece per agent at a time, so a slow agent delays only itself; without
// wait the round returns at once. With all, every registration is observed
// whatever its policy.
func (k *Keeper) round(ctx context.Context, full, wait, all bool) {
	regs, err := k.Engine.Registrations()
	if err != nil {
		k.Log.Printf("keeper: registrations: %v", err)
		return
	}
	var wg *sync.WaitGroup
	if wait {
		wg = &sync.WaitGroup{}
	}
	enabled := k.Enabled()
	seen := map[string]string{}
	for _, r := range regs {
		if ctx.Err() != nil {
			return
		}
		if r.AgentID == "" {
			continue
		}
		path, id, home := r.Path, r.AgentID, k.Engine.ResolveHome(r.Home)
		agentKey := key(home, id)
		seen[agentKey] = path
		step := false
		if enabled {
			if state, err := paseo.ReadObject(path); err == nil {
				if c, err := readCache(state); err == nil && c != nil {
					on := c.Policy != PolicyOff && c.Policy != ""
					step = c.Compaction.inFlight() || full && (on || needsTokensAfter(c))
				}
			}
		}
		_, have := k.cached(home, id)
		switch {
		case step:
			k.dispatch(ctx, agentKey, func(ctx context.Context) { k.Step(ctx, path) }, wg)
		case full || all || !have:
			k.dispatch(ctx, agentKey, func(ctx context.Context) { _, _ = k.observeStore(ctx, id, home) }, wg)
		}
	}
	// Agents whose registration is gone stay readable while Paseo lists
	// them (looked at again without the file); the others are dropped.
	m := k.mem()
	m.mu.Lock()
	gone := []string{}
	for agentKey := range m.regs {
		if _, ok := seen[agentKey]; !ok {
			gone = append(gone, agentKey)
		}
	}
	m.regs = seen
	m.mu.Unlock()
	for _, agentKey := range gone {
		home, id, _ := strings.Cut(agentKey, "\x00")
		if k.isListed(home, id) {
			k.dispatch(ctx, agentKey, func(ctx context.Context) { _, _ = k.observeStore(ctx, id, home) }, wg)
		} else {
			k.forget(home, id, false)
		}
	}
	if wg != nil {
		wg.Wait()
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
	if err != nil {
		return
	}
	k.store(o)
	if o.cache == nil {
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
	k.store(o)
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
	k.refreshPath(path)
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
	if err != nil {
		return
	}
	k.store(fresh)
	if fresh.cache == nil {
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
		k.store(o)
	}
	if o.archived() {
		k.finish(o.path, comp.ID, StateFailed, "archived")
		return
	}
	if err == nil {
		status := o.status()
		idle := status == "idle" || status == "closed"
		lctx, cancel := context.WithTimeout(ctx, observeTimeout)
		items, lerr := k.Paseo.Logs(lctx, o.agentID, o.home, model.Time(comp.SentAt-2))
		cancel()
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

// applyDefault gives the default policy, once, to recently active main
// agents (not subagents) of a listing whose policy was never decided. It decides from the
// observations in memory and looks again (files only) before setting.
// parentLabel is the label Paseo sets on an agent another agent created.
const parentLabel = "paseo.parent-agent-id"

func (k *Keeper) applyDefault(ctx context.Context, home string, list []*paseo.Object) {
	policy := k.Config.Keeper.DefaultPolicy
	if policy == "" || policy == PolicyOff || !k.Enabled() {
		return
	}
	undecided := func(o *obs) bool {
		if o.record == nil || o.archived() || o.record.Value("internal") == true || o.child() {
			return false // subagents: their parent decides
		}
		if o.cache != nil && o.cache.SetAt > 0 {
			return false // decided before
		}
		v := k.view(o)
		return v.realAt > 0 && k.nowS()-v.realAt <= defaultRecent.Seconds()
	}
	for _, item := range list {
		raw, _ := item.String("id")
		id, ok := paseo.CanonicalID(raw)
		if !ok || ctx.Err() != nil {
			continue
		}
		e, ok := k.cached(home, id)
		if !ok || !undecided(e.o) {
			continue
		}
		if o, err := k.observeStore(ctx, id, home); err != nil || !undecided(o) {
			continue
		}
		if _, err := k.setPolicy(ctx, id, home, policy, 0, "default", false); err != nil &&
			!errors.Is(err, ErrBadRequest) {
			k.Log.Printf("keeper %s: default policy: %v", id, err)
		}
	}
}
