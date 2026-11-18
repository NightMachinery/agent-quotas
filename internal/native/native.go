// Package native is the native auto-continue engine: the port of
// agent-auto-continue.zsh and of the delivery half of the armed-job engine in
// agent-usage.zsh. A terminal session (Claude Code, Codex or Antigravity)
// registers itself under a scope, the account whose limit it shares; the
// daemon calls Check whenever that account's state may have changed, and
// Check resumes the sessions that were blocked once the account is usable
// again. See docs/design.md, "Native engine".
//
// Everything external (tmux, kitty, codex, claude, Hammerspoon, ps,
// notifications) goes through procx.Runner and the small interfaces below,
// so tests run against fakes.
package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// DefaultText is the old agent_auto_continue_text: self-limiting, so a wrong
// resume happens once rather than at every reset.
const DefaultText = "Continue. Only if nothing at all remains of your work (no open items, nothing waiting on other agents or on me), say so briefly and run: zsh -c agent-auto-continue-off"

// Default idle thresholds per target kind, in seconds. Only frontmost (and
// any kind without a setting) waits for the keyboard to be idle.
const (
	defaultIdleMinS      = 600
	defaultKickMin       = 60 * time.Second
	defaultBgAttachWait  = 20 * time.Second
	defaultCommandTimout = 30 * time.Second
	hsTimeout            = 10 * time.Second
)

var zeroIdleKinds = map[string]bool{"kitty": true, "tmux": true, "codex": true, "paseo": true, "claude_bg": true}

// Notifier shows a desktop notification.
type Notifier interface {
	Notify(ctx context.Context, title, body string) error
}

// LiveRow is one row of the live session listing (agent_session live-all):
// pid, session id, name, cwd, transcript, tmux session, status, kind.
type LiveRow struct {
	PID        string
	ID         string
	Name       string
	Cwd        string
	Transcript string
	Tmux       string
	Status     string
	Kind       string
}

// LiveLister lists the live agent sessions.
type LiveLister interface {
	Live(ctx context.Context) ([]LiveRow, error)
}

// Engine is the native auto-continue engine. Construct it with New; the
// exported fields may be replaced before first use (tests do).
type Engine struct {
	Cfg      *config.Config
	Runner   procx.Runner
	Notifier Notifier
	Lister   LiveLister

	// Now and Sleep are the clock; tests fix them.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// Binaries, looked up on PATH (cfg.Env may set it).
	Tmux, Kitty, Hammerspoon, Claude, PS string

	// KittySocketGlob finds kitty's remote-control sockets when
	// KITTY_LISTEN_ON does not name a live one. Default:
	// ${NIGHT_SOCKETS_DIR:-~/.local/state}/kitty-*.sock.
	KittySocketGlob string
	// ProcAlive reports whether a pid is running (kitty socket filtering).
	ProcAlive func(pid int) bool

	// KickMin debounces hook kicks per scope (agent_auto_continue_kick_min_s).
	KickMin time.Duration
	// BgAttachWait bounds the wait for a background session's prompt.
	BgAttachWait time.Duration
	// Deliver replaces Send in Fire when set (tests).
	Deliver func(ctx context.Context, target string) error

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New returns an engine for cfg that runs commands through runner.
func New(cfg *config.Config, runner procx.Runner) *Engine {
	e := &Engine{
		Cfg:          cfg,
		Runner:       runner,
		Now:          time.Now,
		Sleep:        sleepCtx,
		Tmux:         "tmux",
		Kitty:        "kitty",
		Hammerspoon:  "hs",
		Claude:       "claude",
		PS:           "ps",
		ProcAlive:    procAlive,
		KickMin:      defaultKickMin,
		BgAttachWait: defaultBgAttachWait,
	}
	e.Notifier = CommandNotifier{Runner: runner, Command: cfg.Notify.Command, Env: e.baseEnv}
	e.Lister = &CommandLister{Engine: e}
	sockDir := os.Getenv("NIGHT_SOCKETS_DIR")
	if sockDir == "" {
		sockDir = filepath.Join(config.Home(), ".local", "state")
	}
	e.KittySocketGlob = filepath.Join(sockDir, "kitty-*.sock")
	return e
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) dir() string { return config.Expand(e.Cfg.Native.RegistrationsDir) }

func (e *Engine) text() string {
	if e.Cfg.Native.Text != "" {
		return e.Cfg.Native.Text
	}
	return DefaultText
}

func (e *Engine) logFile() string {
	if e.Cfg.Native.LogFile != "" {
		return config.Expand(e.Cfg.Native.LogFile)
	}
	return filepath.Join(config.Home(), "logs", "agent-usage-arm.log")
}

// baseEnv is the environment for the commands the engine runs: the daemon's
// own, minus every agent-session marker (a command run for one session must
// not inherit another's), plus cfg.Env.
func (e *Engine) baseEnv() []string {
	env := procx.EnvWithout(os.Environ(), procx.AgentMarkers, nil)
	for k, v := range e.Cfg.Env {
		env = procx.EnvSet(env, k, v)
	}
	return env
}

func (e *Engine) run(ctx context.Context, timeout time.Duration, env []string, argv ...string) (procx.Result, error) {
	if env == nil {
		env = e.baseEnv()
	}
	if timeout == 0 {
		timeout = defaultCommandTimout
	}
	return e.Runner.Run(ctx, procx.Cmd{Argv: argv, Env: env, Timeout: timeout})
}

// scopeLock serializes work on one scope.
func (e *Engine) scopeLock(scope string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.locks == nil {
		e.locks = map[string]*sync.Mutex{}
	}
	l, ok := e.locks[scope]
	if !ok {
		l = &sync.Mutex{}
		e.locks[scope] = l
	}
	return l
}

// ScopeAgent is the agent a scope belongs to: the part before the first "-".
func ScopeAgent(scope string) string {
	a, _, _ := strings.Cut(scope, "-")
	return a
}

// AccountForScope maps a scope to the account id it waits on:
// claude-<profile> -> claude:<profile>, codex-<alias> -> codex:<alias>,
// agy -> the first configured Antigravity account.
func (e *Engine) AccountForScope(scope string) string {
	switch {
	case strings.HasPrefix(scope, "claude-"):
		return model.ID(model.Claude, strings.TrimPrefix(scope, "claude-"))
	case strings.HasPrefix(scope, "codex-"):
		return model.ID(model.Codex, strings.TrimPrefix(scope, "codex-"))
	case scope == "agy":
		name := "m"
		if len(e.Cfg.Agy.Accounts) > 0 {
			name = e.Cfg.Agy.Accounts[0].Name
		}
		return model.ID(model.Agy, name)
	}
	return ""
}

// ScopesForAccount is the inverse of AccountForScope over the scopes that
// currently have registrations.
func (e *Engine) ScopesForAccount(account string) ([]string, error) {
	scopes, err := e.Scopes()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range scopes {
		if e.AccountForScope(s) == account {
			out = append(out, s)
		}
	}
	return out, nil
}

// providerMinInterval is how late the daemon's first sighting of a block can
// be, which widens the activity window (the old watcher's poll interval).
func (e *Engine) providerMinInterval(scope string) int64 {
	switch ScopeAgent(scope) {
	case "claude":
		return int64(e.Cfg.Claude.MinIntervalS)
	case "codex":
		return int64(e.Cfg.Codex.MinIntervalS)
	case "agy":
		return int64(e.Cfg.Agy.MinIntervalS)
	}
	return 300
}
