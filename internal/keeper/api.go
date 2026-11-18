package keeper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/daemon"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// Long-poll limits.
const (
	defaultWait = 60 * time.Second
	maxWait     = 120 * time.Second
	waitRecheck = 5 * time.Second
)

// CompactResult is the answer to a compaction request.
type CompactResult struct {
	CompactionID string `json:"compaction_id"`
	State        string `json:"state"`
}

func canonical(id string) (string, error) {
	c, ok := paseo.CanonicalID(id)
	if !ok {
		return "", fmt.Errorf("%w: an exact agent UUID is required", ErrBadRequest)
	}
	return c, nil
}

// Agents lists every agent with a registration file (policy filters:
// off, compact, warm, any = not off), plus, with all, every agent Paseo
// lists. No paseo inspect.
func (k *Keeper) Agents(ctx context.Context, home, policy string, all bool) ([]View, error) {
	switch policy {
	case "", "any", PolicyOff, PolicyCompact, PolicyWarm:
	default:
		return nil, fmt.Errorf("%w: policy must be off, compact, warm or any", ErrBadRequest)
	}
	regs, err := k.Engine.Registrations()
	if err != nil {
		return nil, err
	}
	out := []View{}
	seen := map[string]bool{}
	resolved := k.Engine.ResolveHome(home)
	for _, r := range regs {
		if home != "" && r.Home != resolved {
			continue
		}
		o, err := k.observe(ctx, r.AgentID, r.Home, false)
		if err != nil {
			continue
		}
		seen[r.AgentID] = true
		v := k.view(o)
		if !policyMatches(v, policy) {
			continue
		}
		out = append(out, v)
	}
	if all {
		list, err := k.Paseo.List(ctx, resolved)
		if err != nil {
			return nil, err
		}
		for _, item := range list {
			raw, _ := item.String("id")
			id, ok := paseo.CanonicalID(raw)
			if !ok || seen[id] {
				continue
			}
			o, err := k.observe(ctx, id, resolved, false)
			if err != nil {
				continue
			}
			v := k.view(o)
			if v.Status == "" {
				v.Status, _ = item.String("status")
			}
			if v.Title == nil {
				if t, ok := item.String("name"); ok && t != "" {
					v.Title = &t
				}
			}
			if policyMatches(v, policy) {
				out = append(out, v)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func policyMatches(v View, policy string) bool {
	p := PolicyOff
	if v.Cache != nil && v.Cache.Policy != "" {
		p = v.Cache.Policy
	}
	switch policy {
	case "":
		return true
	case "any":
		return p != PolicyOff
	}
	return p == policy
}

// Agent is one agent's document, with a fresh paseo inspect when Paseo
// answers.
func (k *Keeper) Agent(ctx context.Context, id, home string) (View, error) {
	id, err := canonical(id)
	if err != nil {
		return View{}, err
	}
	o, err := k.observe(ctx, id, home, true)
	if err != nil && !errors.Is(err, ErrUnknownAgent) {
		o, err = k.observe(ctx, id, home, false)
	}
	if err != nil {
		return View{}, err
	}
	if o.record == nil && o.inspect == nil {
		return View{}, ErrUnknownAgent
	}
	return k.view(o), nil
}

// knownAgent inspects an agent for an acting request: unknown agents give
// ErrUnknownAgent, archived ones a bad request.
func (k *Keeper) knownAgent(ctx context.Context, id, home string) (*paseo.Object, error) {
	snap, err := k.Paseo.Inspect(ctx, id, home)
	if err != nil {
		if _, serr := k.Engine.Stored(id, home); serr != nil {
			return nil, ErrUnknownAgent
		}
		return nil, err
	}
	if got, _ := snap.String("Id"); got != id {
		return nil, ErrUnknownAgent
	}
	if archived, _ := snap.Value("Archived").(bool); archived {
		return nil, fmt.Errorf("%w: the agent is archived", ErrBadRequest)
	}
	return snap, nil
}

// lockedState opens (or, with create, makes) an agent's registration file
// under its lock. state is nil when there is no file and create is false.
func (k *Keeper) lockedState(snap *paseo.Object, id, home string, create bool) (path string, state *paseo.Object, unlock func(), err error) {
	path, err = k.Engine.RegistrationPath(id, home)
	if err != nil {
		return "", nil, nil, err
	}
	unlock, err = paseo.LockFile(paseo.LockPath(path))
	if err != nil {
		return "", nil, nil, err
	}
	state, err = paseo.ReadObject(path)
	if err == nil {
		return path, state, unlock, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		unlock()
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return path, nil, nil, err
	}
	binding, berr := k.Engine.Binding(snap, home)
	if berr != nil {
		unlock()
		return "", nil, nil, fmt.Errorf("%w: the keeper supports agents on Claude or Codex seats (%v)", ErrBadRequest, berr)
	}
	state = paseo.Obj("version", 1, "enabled", false, "home", home, "agent_id", id,
		"binding", binding, "poll", 300, "status", paseo.StatusOff, "registered_at", k.nowS())
	return path, state, unlock, nil
}

func validSetBy(s string, internal bool) (string, error) {
	switch s {
	case "":
		return "desk", nil
	case "agent", "desk", "cli":
		return s, nil
	case "default":
		if internal {
			return s, nil
		}
	}
	return "", fmt.Errorf("%w: set_by must be agent, desk or cli", ErrBadRequest)
}

// SetPolicy sets an agent's cache policy (creating a policy-only file when
// needed) and returns its document.
func (k *Keeper) SetPolicy(ctx context.Context, id, home, policy string, warmHours float64, setBy string) (View, error) {
	return k.setPolicy(ctx, id, home, policy, warmHours, setBy, false)
}

// Standby ends warming: the policy becomes compact, recorded as standby.
func (k *Keeper) Standby(ctx context.Context, id, home, setBy string) (View, error) {
	return k.setPolicy(ctx, id, home, PolicyCompact, 0, setBy, true)
}

func (k *Keeper) setPolicy(ctx context.Context, id, home, policy string, warmHours float64, setBy string, standby bool) (View, error) {
	if !k.Enabled() {
		return View{}, ErrDisabled
	}
	switch policy {
	case PolicyOff, PolicyCompact, PolicyWarm:
	default:
		return View{}, fmt.Errorf("%w: cache must be off, compact or warm", ErrBadRequest)
	}
	if warmHours < 0 {
		return View{}, fmt.Errorf("%w: warm_hours must not be negative", ErrBadRequest)
	}
	setBy, err := validSetBy(setBy, true)
	if err != nil {
		return View{}, err
	}
	if id, err = canonical(id); err != nil {
		return View{}, err
	}
	home = k.Engine.ResolveHome(home)
	snap, err := k.knownAgent(ctx, id, home)
	if err != nil {
		return View{}, err
	}
	create := policy != PolicyOff || standby
	path, state, unlock, err := k.lockedState(snap, id, home, create)
	if err != nil {
		return View{}, err
	}
	if state != nil {
		func() {
			defer unlock()
			var c *Cache
			if c, err = readCache(state); err != nil {
				return
			}
			if c == nil {
				c = &Cache{}
			}
			now := k.nowS()
			c.Policy, c.SetAt, c.SetBy, c.StandbyAt = policy, now, setBy, nil
			if standby {
				c.StandbyAt = &now
			}
			if warmHours > 0 {
				c.WarmHours = warmHours
			} else if c.WarmHours <= 0 {
				c.WarmHours = k.Config.Keeper.WarmHours
			}
			if err = writeCache(state, c); err != nil {
				return
			}
			err = paseo.AtomicWrite(path, state)
		}()
		if err != nil {
			return View{}, err
		}
		what := "policy " + policy
		if standby {
			what = "standby (policy compact)"
		}
		k.Log.Printf("keeper %s: %s, set by %s", id, what, setBy)
		k.signal()
		k.request(path)
	}
	return k.Agent(ctx, id, home)
}

// Compact queues a manual compaction (or returns the one in flight) and
// tries to send it at once.
func (k *Keeper) Compact(ctx context.Context, id, home string) (CompactResult, error) {
	if !k.Enabled() {
		return CompactResult{}, ErrDisabled
	}
	id, err := canonical(id)
	if err != nil {
		return CompactResult{}, err
	}
	home = k.Engine.ResolveHome(home)
	snap, err := k.knownAgent(ctx, id, home)
	if err != nil {
		return CompactResult{}, err
	}
	path, state, unlock, err := k.lockedState(snap, id, home, true)
	if err != nil {
		return CompactResult{}, err
	}
	var res CompactResult
	var replaced string
	func() {
		defer unlock()
		var c *Cache
		if c, err = readCache(state); err != nil {
			return
		}
		if c == nil {
			c = &Cache{Policy: PolicyOff}
		}
		if comp := c.Compaction; comp != nil && comp.State == StateSent {
			res = CompactResult{CompactionID: comp.ID, State: comp.State}
			return
		}
		if comp := c.Compaction; comp != nil && comp.State == StateQueued {
			replaced = comp.ID
		}
		comp := &Compaction{ID: k.newID(), Reason: "manual", State: StateQueued, RequestedAt: k.nowS()}
		c.Compaction = comp
		res = CompactResult{CompactionID: comp.ID, State: comp.State}
		if err = writeCache(state, c); err != nil {
			return
		}
		err = paseo.AtomicWrite(path, state)
	}()
	if err != nil {
		return CompactResult{}, err
	}
	if replaced != "" {
		k.Log.Printf("keeper %s: compaction %s canceled (replaced by %s)", id, replaced, res.CompactionID)
	}
	if res.State == StateQueued {
		k.Log.Printf("keeper %s: compaction %s queued (manual)", id, res.CompactionID)
		k.signal()
		k.Step(ctx, path)
		if st, err := paseo.ReadObject(path); err == nil {
			if c, _ := readCache(st); c != nil && c.Compaction != nil && c.Compaction.ID == res.CompactionID {
				res.State = c.Compaction.State
			}
		}
	}
	return res, nil
}

// WaitCompaction answers when compaction cid of the agent is terminal (or
// no longer the agent's latest), or at the timeout (capped at 120 s).
func (k *Keeper) WaitCompaction(ctx context.Context, id, home, cid string, timeout time.Duration) (View, error) {
	if timeout <= 0 {
		timeout = defaultWait
	}
	if timeout > maxWait {
		timeout = maxWait
	}
	id, err := canonical(id)
	if err != nil {
		return View{}, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		changed := k.changes()
		o, err := k.observe(ctx, id, home, false)
		if err != nil {
			return View{}, err
		}
		v := k.view(o)
		var comp *Compaction
		if v.Cache != nil {
			comp = v.Cache.Compaction
		}
		if comp == nil || comp.ID != cid || comp.Terminal() {
			return v, nil
		}
		recheck := time.NewTimer(waitRecheck)
		select {
		case <-ctx.Done():
			recheck.Stop()
			return View{}, ctx.Err()
		case <-deadline.C:
			recheck.Stop()
			return v, nil
		case <-changed:
		case <-recheck.C:
		}
		recheck.Stop()
	}
}

// ---- HTTP ----

// Mux is where the handlers go (daemon.Server satisfies it).
type Mux interface {
	Handle(pattern string, h http.HandlerFunc)
}

// Document wraps one agent.
type Document struct {
	SchemaVersion int     `json:"schema_version"`
	GeneratedAt   float64 `json:"generated_at"`
	Agent         View    `json:"agent"`
}

// ListDocument wraps many agents.
type ListDocument struct {
	SchemaVersion int     `json:"schema_version"`
	GeneratedAt   float64 `json:"generated_at"`
	Enabled       bool    `json:"keeper_enabled"`
	Agents        []View  `json:"agents"`
}

// PolicyRequest is the body of POST /v1/agents/{id}/policy and .../standby.
type PolicyRequest struct {
	Cache     string  `json:"cache"`
	WarmHours float64 `json:"warm_hours"`
	SetBy     string  `json:"set_by"`
}

func writeErr(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	var pe *paseo.Error
	switch {
	case errors.Is(err, ErrDisabled):
		status, code = http.StatusServiceUnavailable, "keeper_disabled"
	case errors.Is(err, ErrUnknownAgent):
		status, code = http.StatusNotFound, "unknown_agent"
	case errors.Is(err, ErrBadRequest), errors.Is(err, daemon.ErrBadRequest):
		status, code = http.StatusBadRequest, "bad_request"
	case errors.As(err, &pe):
		status, code = http.StatusBadRequest, "refused"
	}
	daemon.WriteJSON(w, status, daemon.ErrorBody{Error: err.Error(), Code: code})
}

// readBody decodes an optional JSON body.
func readBody(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return daemon.ReadJSON(r, v)
}

// Register adds the keeper's routes.
func (k *Keeper) Register(m Mux) {
	m.Handle("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		all := q.Get("all") == "1" || q.Get("all") == "true"
		agents, err := k.Agents(r.Context(), q.Get("home"), q.Get("policy"), all)
		if err != nil {
			writeErr(w, err)
			return
		}
		daemon.WriteJSON(w, http.StatusOK, ListDocument{SchemaVersion: model.SchemaVersion,
			GeneratedAt: k.nowS(), Enabled: k.Enabled(), Agents: agents})
	})
	m.Handle("GET /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var v View
		var err error
		if cid := q.Get("wait_compaction"); cid != "" {
			secs, _ := strconv.ParseFloat(q.Get("timeout"), 64)
			v, err = k.WaitCompaction(r.Context(), r.PathValue("id"), q.Get("home"), cid, time.Duration(secs*float64(time.Second)))
		} else {
			v, err = k.Agent(r.Context(), r.PathValue("id"), q.Get("home"))
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		daemon.WriteJSON(w, http.StatusOK, Document{SchemaVersion: model.SchemaVersion, GeneratedAt: k.nowS(), Agent: v})
	})
	m.Handle("POST /v1/agents/{id}/policy", func(w http.ResponseWriter, r *http.Request) {
		var req PolicyRequest
		if err := readBody(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		setBy, err := validSetBy(req.SetBy, false)
		if err != nil {
			writeErr(w, err)
			return
		}
		v, err := k.SetPolicy(r.Context(), r.PathValue("id"), r.URL.Query().Get("home"), req.Cache, req.WarmHours, setBy)
		if err != nil {
			writeErr(w, err)
			return
		}
		daemon.WriteJSON(w, http.StatusOK, Document{SchemaVersion: model.SchemaVersion, GeneratedAt: k.nowS(), Agent: v})
	})
	m.Handle("POST /v1/agents/{id}/standby", func(w http.ResponseWriter, r *http.Request) {
		var req PolicyRequest
		if err := readBody(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		setBy, err := validSetBy(req.SetBy, false)
		if err != nil {
			writeErr(w, err)
			return
		}
		v, err := k.Standby(r.Context(), r.PathValue("id"), r.URL.Query().Get("home"), setBy)
		if err != nil {
			writeErr(w, err)
			return
		}
		daemon.WriteJSON(w, http.StatusOK, Document{SchemaVersion: model.SchemaVersion, GeneratedAt: k.nowS(), Agent: v})
	})
	m.Handle("POST /v1/agents/{id}/compact", func(w http.ResponseWriter, r *http.Request) {
		res, err := k.Compact(r.Context(), r.PathValue("id"), r.URL.Query().Get("home"))
		if err != nil {
			writeErr(w, err)
			return
		}
		daemon.WriteJSON(w, http.StatusOK, res)
	})
}
