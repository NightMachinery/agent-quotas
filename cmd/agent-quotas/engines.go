package main

import (
	"context"
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
// becomes usable, and on request.
type paseoScheduler struct {
	eng  *paseo.Engine
	log  *log.Logger
	kick chan paseoKick

	mu   sync.Mutex
	last map[string]time.Time
}

type paseoKick struct {
	path    string // one registration, or "" with account/all
	account string // ticks registrations bound to this account id
	all     bool
}

func newPaseoScheduler(eng *paseo.Engine, logger *log.Logger) *paseoScheduler {
	return &paseoScheduler{eng: eng, log: logger, kick: make(chan paseoKick, 64), last: map[string]time.Time{}}
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
	ticker := time.NewTicker(15 * time.Second)
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
	for _, r := range regs {
		if !r.Enabled || ctx.Err() != nil {
			continue
		}
		due := false
		switch {
		case k.all, k.path != "" && k.path == r.Path:
			due = true
		case k.account != "":
			b := boundAccount(r)
			p, _, _ := model.SplitID(k.account)
			due = b == k.account || (b == model.Codex && p == model.Codex)
		}
		s.mu.Lock()
		last, seen := s.last[r.Path]
		s.mu.Unlock()
		poll := time.Duration(r.Poll) * time.Second
		if poll < 30*time.Second {
			poll = 30 * time.Second
		}
		if !seen || now.Sub(last) >= poll {
			due = true
		}
		if !due {
			continue
		}
		s.mu.Lock()
		s.last[r.Path] = now
		s.mu.Unlock()
		status, err := s.eng.Tick(ctx, r.Path)
		if err != nil {
			s.log.Printf("paseo %s: check unavailable; no message sent (%v)", r.AgentID, err)
			continue
		}
		if status != r.Status {
			s.log.Printf("paseo %s: %s", r.AgentID, status)
		}
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
	if cfg.Paseo.Enabled == nil || *cfg.Paseo.Enabled {
		peng = paseo.New(cfg, quotas)
		ps = newPaseoScheduler(peng, logger)
		runs = append(runs, ps.run)
	}
	ns, neng := attachNative(cfg, d, quotas, logger)
	if ns != nil {
		runs = append(runs, ns.run)
	}
	runs = append(runs, attachKeeper(cfg, d, srv, quotas, logger))

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
