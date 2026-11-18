package keeper

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// The read API answers from memory: every GET is served from the last
// observation of each agent, which the observation loop (Run) and the
// keeper's own writes keep current. No GET runs an external command. See
// docs/design.md, "Serving reads from memory".

// Observation bounds.
const (
	// observeTimeout bounds the external commands of one agent's
	// observation (paseo inspect, paseo logs).
	observeTimeout = 30 * time.Second
	// observeWorkers is how many agents are observed (or stepped) at once.
	observeWorkers = 8
	// minContextEvery is how often the compaction file is re-read.
	minContextEvery = 10 * time.Second
	// primeWait is how long reads wait for the first observation of every
	// registration at start; slower agents fill in later.
	primeWait = 5 * time.Second
	// missWait is how long a read waits for the first observation of an
	// agent it has none for.
	missWait = 2 * time.Second
)

// entry is one agent's last observation.
type entry struct {
	o  *obs
	at float64 // when it was observed (Unix seconds)
}

// listing is the last `paseo ls` of one Paseo home.
type listing struct {
	at    float64
	items []*paseo.Object
}

// memory is the keeper's in-memory state for reads.
type memory struct {
	mu       sync.RWMutex
	agents   map[string]*entry   // key(home, id) -> last observation
	regs     map[string]string   // key(home, id) -> registration path, as of the last round
	unknown  map[string]bool     // key(home, id) -> Paseo did not know it at the last look
	lists    map[string]*listing // resolved home -> last paseo ls
	primed   bool
	primeMu  sync.Mutex
	minMu    sync.Mutex
	minCtx   int64
	minCtxAt time.Time

	runMu   sync.Mutex
	running map[string]bool                  // key -> work in progress
	again   map[string]func(context.Context) // key -> work queued behind it
	sem     chan struct{}
}

func key(home, id string) string { return home + "\x00" + id }

func (k *Keeper) mem() *memory {
	k.memOnce.Do(func() {
		k.m = &memory{agents: map[string]*entry{}, regs: map[string]string{}, unknown: map[string]bool{},
			lists:   map[string]*listing{},
			running: map[string]bool{}, again: map[string]func(context.Context){},
			sem: make(chan struct{}, observeWorkers)}
	})
	return k.m
}

// minContext is MinContextTokens of the configured file, re-read at most
// every minContextEvery.
func (k *Keeper) minContext() int64 {
	m := k.mem()
	m.minMu.Lock()
	defer m.minMu.Unlock()
	if m.minCtxAt.IsZero() || time.Since(m.minCtxAt) >= minContextEvery {
		m.minCtx, m.minCtxAt = MinContextTokens(k.Config.Keeper.CompactionFile), time.Now()
	}
	return m.minCtx
}

// store records an observation.
func (k *Keeper) store(o *obs) {
	if o == nil {
		return
	}
	m := k.mem()
	m.mu.Lock()
	m.agents[key(o.home, o.agentID)] = &entry{o: o, at: k.nowS()}
	delete(m.unknown, key(o.home, o.agentID))
	m.mu.Unlock()
}

// forget drops an agent's observation; unknown records that Paseo does
// not know it (so a registration of it is not listed).
func (k *Keeper) forget(home, id string, unknown bool) {
	m := k.mem()
	m.mu.Lock()
	delete(m.agents, key(home, id))
	if unknown {
		m.unknown[key(home, id)] = true
	}
	m.mu.Unlock()
}

// noteRegistration marks an agent as having a registration file (until the
// next round says otherwise).
func (k *Keeper) noteRegistration(home, id, path string) {
	m := k.mem()
	m.mu.Lock()
	m.regs[key(home, id)] = path
	m.mu.Unlock()
}

func (k *Keeper) cached(home, id string) (*entry, bool) {
	m := k.mem()
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.agents[key(home, id)]
	return e, ok
}

// observeStore observes one agent from its files only (no external
// command) and records the result.
func (k *Keeper) observeStore(ctx context.Context, id, home string) (*obs, error) {
	o, err := k.observe(ctx, id, home, false)
	if err != nil {
		if err == ErrUnknownAgent {
			k.forget(k.Engine.ResolveHome(home), id, true)
		}
		return nil, err
	}
	k.store(o)
	return o, nil
}

// refreshPath re-observes the agent of a registration file after the keeper
// wrote it, so reads and long-polls see the change.
func (k *Keeper) refreshPath(path string) {
	state, err := paseo.ReadObject(path)
	if err != nil {
		return
	}
	id, _ := state.String("agent_id")
	home, _ := state.String("home")
	if id == "" {
		return
	}
	if o, err := k.observeStore(context.Background(), id, home); err == nil {
		k.noteRegistration(o.home, id, path)
	}
}

// viewOf derives an agent's document from its last observation. The data
// fields are as of observed_at; the time-dependent ones (phase,
// next_action, sendable) are evaluated now. No I/O.
func (k *Keeper) viewOf(e *entry) View {
	v := k.view(e.o)
	at := e.at
	v.ObservedAt = &at
	return v
}

// ---- dispatching observation work ----

// dispatch runs fn for one agent on the worker pool, unless work for that
// agent is already running, in which case fn runs once after it. wg, when
// set, is done when this call's work (or its queueing) is.
func (k *Keeper) dispatch(ctx context.Context, agentKey string, fn func(context.Context), wg *sync.WaitGroup) {
	m := k.mem()
	m.runMu.Lock()
	if m.running[agentKey] {
		m.again[agentKey] = fn
		m.runMu.Unlock()
		return
	}
	m.running[agentKey] = true
	m.runMu.Unlock()
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		for fn != nil {
			select {
			case m.sem <- struct{}{}:
			case <-ctx.Done():
				m.runMu.Lock()
				delete(m.running, agentKey)
				delete(m.again, agentKey)
				m.runMu.Unlock()
				return
			}
			fn(ctx)
			<-m.sem
			m.runMu.Lock()
			fn = m.again[agentKey]
			delete(m.again, agentKey)
			if fn == nil {
				delete(m.running, agentKey)
			}
			m.runMu.Unlock()
		}
	}()
}

// prime observes every registration once, from files only, before the
// first read is answered (reads wait for it).
func (k *Keeper) prime(ctx context.Context) {
	m := k.mem()
	m.primeMu.Lock()
	defer m.primeMu.Unlock()
	m.mu.RLock()
	primed := m.primed
	m.mu.RUnlock()
	if primed {
		return
	}
	done := make(chan struct{})
	go func() {
		// Not tied to the first reader's request.
		k.round(context.WithoutCancel(ctx), false, true, true)
		close(done)
	}()
	timer := time.NewTimer(primeWait)
	select {
	case <-done:
	case <-timer.C:
		k.Log.Printf("keeper: first observation still running after %s; answering reads meanwhile", primeWait)
	case <-ctx.Done():
	}
	timer.Stop()
	m.mu.Lock()
	m.primed = true
	m.mu.Unlock()
}

// ---- the listing of every agent ----

// RefreshListing runs `paseo ls` for every configured home, observes the
// agents it lists that have no registration (files only), and then, when
// the keeper is enabled, applies the default policy. Run calls it at start
// and every five minutes; reads with all=1 serve its last result.
func (k *Keeper) RefreshListing(ctx context.Context) {
	for _, h := range k.Config.Paseo.Homes {
		home := k.Engine.ResolveHome(h)
		lctx, cancel := context.WithTimeout(ctx, 2*observeTimeout)
		list, err := k.Paseo.List(lctx, home)
		cancel()
		if err != nil {
			k.Log.Printf("keeper: paseo ls: %v", err)
			continue
		}
		m := k.mem()
		m.mu.Lock()
		m.lists[home] = &listing{at: k.nowS(), items: list}
		// Drop agents of this home that are neither registered nor listed
		// any more (archived or deleted).
		listedNow := map[string]bool{}
		for _, item := range list {
			if raw, _ := item.String("id"); raw != "" {
				if id, ok := paseo.CanonicalID(raw); ok {
					listedNow[key(home, id)] = true
				}
			}
		}
		for agentKey := range m.agents {
			h, _, _ := strings.Cut(agentKey, "\x00")
			if _, reg := m.regs[agentKey]; h == home && !reg && !listedNow[agentKey] {
				delete(m.agents, agentKey)
			}
		}
		m.mu.Unlock()
		var wg sync.WaitGroup
		for _, item := range list {
			raw, _ := item.String("id")
			id, ok := paseo.CanonicalID(raw)
			if !ok || ctx.Err() != nil {
				continue
			}
			m.mu.RLock()
			_, reg := m.regs[key(home, id)]
			m.mu.RUnlock()
			if reg {
				continue // the rounds observe it
			}
			k.dispatch(ctx, key(home, id), func(ctx context.Context) { _, _ = k.observeStore(ctx, id, home) }, &wg)
		}
		wg.Wait()
		if k.Enabled() {
			k.applyDefault(ctx, home, list)
		}
	}
}

// listed is the last listing of home, nil before the first.
func (k *Keeper) listed(home string) *listing {
	m := k.mem()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lists[home]
}

// ListedAt is when the listing of home that all=1 serves was taken, nil
// before the first.
func (k *Keeper) ListedAt(home string) *float64 {
	l := k.listed(k.Engine.ResolveHome(home))
	if l == nil {
		return nil
	}
	at := l.at
	return &at
}

// itemView is the document of a listed agent not observed yet: what the
// listing says.
func (k *Keeper) itemView(id, home string, item *paseo.Object) View {
	v := k.view(&obs{agentID: id, home: home})
	v.Status, _ = item.String("status")
	if t, ok := item.String("name"); ok && t != "" {
		v.Title = &t
	}
	v.Provider, _ = item.String("provider")
	return v
}

// isListed reports whether the last listing of home has the agent.
func (k *Keeper) isListed(home, id string) bool {
	l := k.listed(home)
	if l == nil {
		return false
	}
	for _, item := range l.items {
		if raw, _ := item.String("id"); raw != "" {
			if c, ok := paseo.CanonicalID(raw); ok && c == id {
				return true
			}
		}
	}
	return false
}
