package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/daemon"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// engineQuotaMaxAge is how old a reading may be before an engine acting on
// it asks for a fresh one (subject to the floor and back-off).
const engineQuotaMaxAge = 60 * time.Second

// paseoScheduler runs Paseo registrations' ticks: at start-up, on every
// registration's own poll interval (the safety tick), when its account
// becomes usable, at the reset an agy agent's quota notice names (its
// registration's wake_at), and on request.
type paseoScheduler struct {
	eng  *paseo.Engine
	log  *log.Logger
	kick chan paseoKick

	mu     sync.Mutex
	checks map[string]*paseoCheck
}

type paseoCheck struct {
	next    time.Time
	retry   paseo.CheckRetry
	running bool
	pending bool
	// woke is the wake_at already acted on, so each wakes one tick.
	woke float64
}

type paseoKick struct {
	path    string // one registration, or "" with account/all
	account string // ticks registrations bound to this account id
	all     bool
}

func newPaseoScheduler(eng *paseo.Engine, logger *log.Logger) *paseoScheduler {
	return &paseoScheduler{eng: eng, log: logger, kick: make(chan paseoKick, 64), checks: map[string]*paseoCheck{}}
}

func (s *paseoScheduler) request(k paseoKick) {
	select {
	case s.kick <- k:
	default:
		s.log.Printf("paseo: check queue full; dropped %+v", k)
	}
}

// boundAccount is the account a registration's quota is read from, as far
// as the scheduler needs it: Codex registrations match any Codex account,
// since which auth is active is decided at check time.
func boundAccount(r paseo.Registration) string {
	switch r.Agent {
	case model.Claude:
		return model.ID(model.Claude, r.Profile)
	case model.Codex:
		return model.Codex
	}
	return ""
}

func (s *paseoScheduler) run(ctx context.Context) {
	s.request(paseoKick{all: true})
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		var k paseoKick
		select {
		case <-ctx.Done():
			return
		case k = <-s.kick:
		case <-ticker.C:
		}
		s.round(ctx, k)
	}
}

// round ticks the registrations a kick selects, plus every registration
// whose safety tick is due.
func (s *paseoScheduler) round(ctx context.Context, k paseoKick) {
	regs, err := s.eng.Registrations()
	if err != nil {
		s.log.Printf("paseo: registrations: %v", err)
		return
	}
	now := time.Now()
	batches := map[string]*paseo.PollListing{}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool, len(regs))
	for _, r := range regs {
		if !r.Enabled || ctx.Err() != nil {
			continue
		}
		seen[r.Path] = true
		forced := k.all || k.path != "" && k.path == r.Path
		if k.account != "" {
			b := boundAccount(r)
			p, _, _ := model.SplitID(k.account)
			forced = forced || b == k.account || b == model.Codex && p == model.Codex
		}
		check := s.checks[r.Path]
		if check == nil {
			check = &paseoCheck{}
			s.checks[r.Path] = check
		}
		wake := r.Status == paseo.StatusWaitingQuota && r.WakeAt > 0 && r.WakeAt != check.woke &&
			float64(now.UnixNano())/1e9 >= r.WakeAt
		forced = forced || wake
		if check.running {
			check.pending = check.pending || forced
			if wake {
				check.woke = r.WakeAt
			}
			continue
		}
		if now.Before(check.retry.At) || (!forced && now.Before(check.next)) {
			continue
		}
		if wake {
			check.woke = r.WakeAt
		}
		home := s.eng.ResolveHome(r.Home)
		batch := batches[home]
		if batch == nil {
			batch = &paseo.PollListing{}
			batches[home] = batch
		}
		check.running = true
		go s.check(ctx, r, check, batch)
	}
	for path, check := range s.checks {
		if !seen[path] && !check.running {
			delete(s.checks, path)
		}
	}
}

func (s *paseoScheduler) check(ctx context.Context, r paseo.Registration, check *paseoCheck, batch *paseo.PollListing) {
	status, err := s.eng.Poll(ctx, r.Path, batch)
	poll := max(30*time.Second, time.Duration(r.Poll)*time.Second)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	check.running = false
	check.next = now.Add(poll)
	if err != nil && status != paseo.StatusUncertain && errors.Is(err, paseo.ErrCommandUnavailable) {
		if check.retry.Failed(now, poll) && ctx.Err() == nil {
			s.log.Printf("paseo %s: check unavailable; no message sent (%v)", r.AgentID, err)
		}
		check.next = check.retry.At
		check.pending = false
		return
	}
	check.retry = paseo.CheckRetry{}
	if check.pending {
		check.next = now
		check.pending = false
	}
	if err != nil && ctx.Err() == nil {
		if status == paseo.StatusUncertain {
			s.log.Printf("paseo %s: %s (%v)", r.AgentID, status, err)
		} else {
			s.log.Printf("paseo %s: check unavailable; no message sent (%v)", r.AgentID, err)
		}
	} else if err == nil && status != r.Status {
		s.log.Printf("paseo %s: %s", r.AgentID, status)
	}
}

// ContinueCheckRequest is the body of POST /v1/continue/check.
type ContinueCheckRequest struct {
	Kind  string `json:"kind"`  // paseo | native
	Path  string `json:"path"`  // paseo: one registration file
	Scope string `json:"scope"` // native: one scope
}

// attachEngines wires the auto-continue engines into the daemon: transition
// subscriptions, API routes, and their schedulers (returned to run).
func attachEngines(ctx context.Context, cfg *config.Config, d *daemon.Daemon, srv *daemon.Server, logger *log.Logger) (func(context.Context), error) {
	quotas := quotaAdapter{cfg: cfg, snap: d.MaxAge, maxAge: engineQuotaMaxAge}
	var runs []func(context.Context)

	var ps *paseoScheduler
	var peng *paseo.Engine
	if cfg.Paseo.AutoContinueEnabled() {
		peng = paseo.New(cfg, quotas)
		ps = newPaseoScheduler(peng, logger)
		runs = append(runs, ps.run)
	}
	ns, neng := attachNative(cfg, d, quotas, logger)
	if ns != nil {
		runs = append(runs, ns.run)
	}
	runs = append(runs, attachKeeper(cfg, d, srv, quotas, logger, func(path string) {
		if ps != nil {
			ps.request(paseoKick{path: path})
		}
	}))

	d.Subscribe(func(t model.Transition, _ model.Snapshot) {
		if t.To != model.Usable {
			if ns != nil {
				ns.transition(t)
			}
			return
		}
		if ps != nil {
			ps.request(paseoKick{account: t.ID})
		}
		if ns != nil {
			ns.transition(t)
		}
	})

	srv.Handle("GET /v1/continue", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"schema_version": model.SchemaVersion}
		if peng != nil {
			regs, err := peng.Registrations()
			if err != nil {
				daemon.WriteError(w, err)
				return
			}
			if regs == nil {
				regs = []paseo.Registration{}
			}
			out["paseo"] = regs
		}
		if neng != nil {
			out["native"] = nativeListing(neng)
		}
		daemon.WriteJSON(w, http.StatusOK, out)
	})
	srv.Handle("POST /v1/continue/check", func(w http.ResponseWriter, r *http.Request) {
		var req ContinueCheckRequest
		if err := daemon.ReadJSON(r, &req); err != nil {
			daemon.WriteError(w, err)
			return
		}
		switch req.Kind {
		case "paseo":
			if ps == nil {
				daemon.WriteJSON(w, http.StatusOK, map[string]any{"queued": false, "reason": "paseo engine disabled"})
				return
			}
			ps.request(paseoKick{path: req.Path, all: req.Path == ""})
		case "native":
			if ns == nil {
				daemon.WriteJSON(w, http.StatusOK, map[string]any{"queued": false, "reason": "native engine disabled"})
				return
			}
			ns.request(req.Scope)
		default:
			daemon.WriteError(w, daemon.ErrBadRequest)
			return
		}
		daemon.WriteJSON(w, http.StatusOK, map[string]any{"queued": true})
	})

	if len(runs) == 0 {
		return nil, nil
	}
	return func(ctx context.Context) {
		var wg sync.WaitGroup
		for _, run := range runs {
			wg.Add(1)
			go func(run func(context.Context)) { defer wg.Done(); run(ctx) }(run)
		}
		wg.Wait()
	}, nil
}
