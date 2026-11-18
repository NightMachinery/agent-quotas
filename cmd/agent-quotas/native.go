package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/daemon"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/native"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// nativeScheduler runs native scopes' checks: at start-up, on a safety
// tick, when the scope's account changes verdict, when a check asked to be
// retried (the end of a grace), and on request (hooks, registrations).
type nativeScheduler struct {
	eng  *native.Engine
	d    *daemon.Daemon
	log  *log.Logger
	kick chan string

	mu    sync.Mutex
	last  map[string]time.Time
	retry map[string]time.Time
}

// nativeSafetyTick is how often every scope is checked without a trigger.
const nativeSafetyTick = 5 * time.Minute

func attachNative(cfg *config.Config, d *daemon.Daemon, _ quotaAdapter, logger *log.Logger) (*nativeScheduler, *native.Engine) {
	if cfg.Native.Enabled == nil || !*cfg.Native.Enabled {
		return nil, nil
	}
	eng := native.New(cfg, procx.Exec{})
	return &nativeScheduler{eng: eng, d: d, log: logger, kick: make(chan string, 64),
		last: map[string]time.Time{}, retry: map[string]time.Time{}}, eng
}

func (s *nativeScheduler) request(scope string) {
	select {
	case s.kick <- scope:
	default:
		s.log.Printf("native: check queue full; dropped %q", scope)
	}
}

func (s *nativeScheduler) transition(t model.Transition) {
	scopes, err := s.eng.ScopesForAccount(t.ID)
	if err != nil {
		s.log.Printf("native: scopes of %s: %v", t.ID, err)
		return
	}
	for _, sc := range scopes {
		s.request(sc)
	}
}

func (s *nativeScheduler) run(ctx context.Context) {
	s.request("")
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		var asked string
		explicit := false
		select {
		case <-ctx.Done():
			return
		case asked = <-s.kick:
			explicit = true
		case <-ticker.C:
		}
		scopes, err := s.eng.Scopes()
		if err != nil {
			s.log.Printf("native: scopes: %v", err)
			continue
		}
		now := time.Now()
		for _, sc := range scopes {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			last, seen := s.last[sc]
			retry, hasRetry := s.retry[sc]
			s.mu.Unlock()
			due := !seen || now.Sub(last) >= nativeSafetyTick || (hasRetry && !now.Before(retry)) ||
				(explicit && (asked == "" || asked == sc))
			if due {
				s.check(ctx, sc, explicit || hasRetry)
			}
		}
	}
}

// check runs one scope's check. fresh asks for a reading at most a minute
// old before acting (subject to the floor and back-off).
func (s *nativeScheduler) check(ctx context.Context, scope string, fresh bool) {
	s.mu.Lock()
	s.last[scope] = time.Now()
	delete(s.retry, scope)
	s.mu.Unlock()
	account := s.eng.AccountForScope(scope)
	var snap model.Snapshot
	var err error
	if fresh {
		snap, err = s.d.MaxAge(ctx, account, engineQuotaMaxAge)
	} else {
		snap, err = s.d.Get(ctx, account, false)
	}
	if err != nil && !errors.Is(err, daemon.ErrBackedOff) {
		s.log.Printf("native %s: %s: %v", scope, account, err)
		return
	}
	q := nativeQuota(snap)
	dec, err := s.eng.Check(ctx, scope, q)
	if err != nil {
		s.log.Printf("native %s: check: %v", scope, err)
		return
	}
	if dec.RetryAt != nil {
		s.mu.Lock()
		s.retry[scope] = model.Time(*dec.RetryAt)
		s.mu.Unlock()
	}
	for _, f := range dec.Fires {
		s.log.Printf("native %s: fired %+v", scope, f)
	}
	if len(dec.Pruned) > 0 {
		s.log.Printf("native %s: pruned %s", scope, strings.Join(dec.Pruned, " "))
	}
}

// nativeQuota turns a snapshot into the engine's input. A stale reading is
// unknown, never usable; model blocks matter for Claude families only.
func nativeQuota(snap model.Snapshot) native.ScopeQuota {
	q := native.QuotaFromSnapshot(snap)
	if snap.Stale && snap.Error != nil {
		q.Verdict = model.Unknown
	}
	if snap.Provider != model.Claude {
		q.ModelBlocks = nil
	}
	return q
}

func nativeListing(eng *native.Engine) any {
	out := map[string][]native.Registration{}
	scopes, err := eng.Scopes()
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	for _, sc := range scopes {
		regs, err := eng.Registrations(sc)
		if err == nil {
			out[sc] = regs
		}
	}
	return out
}

func environMap() map[string]string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// nativeCommand is continue on|off|status for the session this shell runs in.
func nativeCommand(ctx context.Context, cfg *config.Config, action string, o nativeOptions) int {
	eng := native.New(cfg, procx.Exec{})
	env := environMap()
	opts := native.OnOptions{Frontmost: o.frontmost}
	switch action {
	case "on":
		res, err := eng.On(ctx, env, opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent-auto-continue: "+err.Error())
			return exitFail
		}
		fmt.Println(res.Message)
		if cfg.Native.Enabled == nil || !*cfg.Native.Enabled {
			fmt.Fprintln(os.Stderr, "note: native.enabled is off in the agent-quotas config, so the daemon does not resume native sessions yet")
		}
		askCheck(ctx, ContinueCheckRequest{Kind: "native", Scope: res.Registration.Scope})
	case "off":
		res, err := eng.Off(env, opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent-auto-continue: "+err.Error())
			return exitFail
		}
		fmt.Println(res.Message)
	default:
		res, err := eng.Status(env, opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent-auto-continue: "+err.Error())
			return exitFail
		}
		fmt.Println(res.Format())
	}
	return exitOK
}

func nativeList(ctx context.Context, cfg *config.Config, prune bool) int {
	ls, err := native.New(cfg, procx.Exec{}).List(ctx, prune)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-auto-continue-list: "+err.Error())
		return exitFail
	}
	if len(ls) == 0 {
		return exitOK // the Paseo rows above stay machine-readable
	}
	if out := native.FormatList(ls); out != "" {
		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Println()
		}
	}
	return exitOK
}

// cmdHook is the agents' hook: hook claude|codex|agy [PANE] < payload. It
// marks a registered session whose turn ended on the limit and asks the
// daemon to check its scope. agy needs a JSON answer on stdout.
func cmdHook(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agent-quotas hook claude|codex|agy [PANE] < payload")
		return exitUsage
	}
	agent, pane := args[0], ""
	if len(args) > 1 {
		pane = args[1]
	}
	if agent == model.Agy {
		defer fmt.Println("{}")
	}
	payload, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	cfg, err := loadConfig()
	if err != nil {
		return exitOK // a hook must never fail its agent's turn
	}
	res, err := native.New(cfg, procx.Exec{}).Hook(ctx, agent, pane, payload)
	if err != nil || res.Scope == "" {
		return exitOK
	}
	_ = newClient(10*time.Second).Do(ctx, "POST", "/v1/continue/check", ContinueCheckRequest{Kind: "native", Scope: res.Scope}, nil)
	return exitOK
}
