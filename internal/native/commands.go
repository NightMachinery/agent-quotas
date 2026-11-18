package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OnOptions tune a registration.
type OnOptions struct {
	// Agent overrides the agent read from the environment.
	Agent string
	// SessionID overrides the id read from the environment.
	SessionID string
	// Transcript is the session's transcript when the caller knows it;
	// otherwise it is resolved from the id.
	Transcript string
	// Frontmost registers the frontmost target (blind typing).
	Frontmost bool
	// Outside: the session was picked from another shell.
	Outside bool
}

// Here is the session an environment names: agent, id, transcript, scope.
type Here struct {
	Agent      string `json:"agent"`
	ID         string `json:"id"`
	Transcript string `json:"transcript"`
	Scope      string `json:"scope"`
}

// HereOf is h-agent-auto-continue-here: the front half of on, off and
// status. An environment inside a Paseo agent returns ErrPaseoAgent.
func (e *Engine) HereOf(env map[string]string, opts OnOptions) (Here, error) {
	if env["PASEO_AGENT_ID"] != "" && opts.Agent == "" {
		return Here{}, ErrPaseoAgent
	}
	agent := opts.Agent
	if agent == "" {
		a, err := AgentOf(env)
		if err != nil {
			return Here{}, err
		}
		agent = a
	}
	id := opts.SessionID
	if id == "" {
		v, err := SessionIDOf(agent, env)
		if err != nil {
			return Here{}, err
		}
		id = v
	}
	if !validName(id) {
		return Here{}, fmt.Errorf("invalid session id: %q", id)
	}
	transcript := opts.Transcript
	if transcript == "" {
		transcript = ResolveTranscript(agent, id, env)
	}
	scope, err := e.ScopeOf(agent, env)
	if err != nil {
		return Here{}, err
	}
	if !validName(scope) {
		return Here{}, fmt.Errorf("invalid scope: %q", scope)
	}
	return Here{Agent: agent, ID: id, Transcript: transcript, Scope: scope}, nil
}

// OnResult is a registration and the line the shell printed for it.
type OnResult struct {
	Registration Registration `json:"registration"`
	Message      string       `json:"message"`
}

// On is agent-auto-continue-on: register the session env names, with its
// targets resolved now. The daemon runs Check for the scope afterwards.
func (e *Engine) On(ctx context.Context, env map[string]string, opts OnOptions) (OnResult, error) {
	h, err := e.HereOf(env, opts)
	if err != nil {
		return OnResult{}, err
	}
	targets, err := e.ResolveTargets(ctx, h.Agent, h.ID, h.Transcript, env, TargetOptions{Frontmost: opts.Frontmost, Outside: opts.Outside})
	if err != nil {
		return OnResult{}, err
	}
	return e.Register(h, targets)
}

// Register is h-agent-auto-continue-register: record one session's
// registration with the given targets.
func (e *Engine) Register(h Here, targets []string) (OnResult, error) {
	targets = uniqueInOrder(targets)
	if len(targets) == 0 {
		return OnResult{}, fmt.Errorf("no target to resume %s %s through", h.Agent, h.ID)
	}
	for _, t := range targets {
		if strings.ContainsAny(t, " \t\n") {
			return OnResult{}, fmt.Errorf("invalid target %q", t)
		}
	}
	r := Registration{Scope: h.Scope, Agent: h.Agent, ID: h.ID, Transcript: h.Transcript, Targets: targets, RegisteredAt: e.now().Unix()}
	l := e.scopeLock(h.Scope)
	l.Lock()
	defer l.Unlock()
	if err := e.writeRegistration(r); err != nil {
		return OnResult{}, err
	}
	msg := fmt.Sprintf("auto-continue on: %s, resumes %s when the limit resets; the agent-quotas daemon checks it whenever %s changes",
		h.Scope, strings.Join(targets, ", "), e.AccountForScope(h.Scope))
	return OnResult{Registration: r, Message: msg}, nil
}

// OffResult is what Off did.
type OffResult struct {
	Scope   string `json:"scope"`
	Removed bool   `json:"removed"`
	Left    int    `json:"left"`
	Message string `json:"message"`
}

// Off is agent-auto-continue-off for the session env names.
func (e *Engine) Off(env map[string]string, opts OnOptions) (OffResult, error) {
	h, err := e.HereOf(env, opts)
	if err != nil {
		return OffResult{}, err
	}
	return e.Unregister(h.Scope, h.Agent, h.ID, "this session")
}

// Unregister is h-agent-auto-continue-unregister: forget one registration;
// with nothing left in the scope, clear its stamps and remove its directory.
// who names the session in the messages.
func (e *Engine) Unregister(scope, agent, id, who string) (OffResult, error) {
	if who == "" {
		who = "this session"
	}
	res := OffResult{Scope: scope}
	if !validName(scope) || !validName(agent) || !validName(id) {
		return res, fmt.Errorf("invalid registration: %s %s-%s", scope, agent, id)
	}
	l := e.scopeLock(scope)
	l.Lock()
	defer l.Unlock()
	file := e.regFile(scope, agent, id)
	if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
		res.Message = "auto-continue was not on for " + who
		return res, nil
	}
	if err := os.Remove(file); err != nil {
		return res, err
	}
	res.Removed = true
	_ = os.Remove(e.markFile(scope, agent, id))
	left, err := e.Registrations(scope)
	if err != nil {
		return res, err
	}
	res.Left = len(left)
	if len(left) == 0 {
		dir := e.scopeDir(scope)
		e.unblocked(scope)
		_ = os.Remove(filepath.Join(dir, kickedFile))
		_ = os.Remove(filepath.Join(dir, fnRanFile))
		_ = os.Remove(dir)
		res.Message = fmt.Sprintf("auto-continue off: %s has no registered session left", scope)
	} else {
		res.Message = fmt.Sprintf("auto-continue off for %s; %s still has %d registered", who, scope, len(left))
	}
	return res, nil
}

// StatusResult is this session's registration and its scope's cycle.
type StatusResult struct {
	Here         Here          `json:"here"`
	Registration *Registration `json:"registration,omitempty"`
	BlockedSince *int64        `json:"blocked_since,omitempty"`
}

// Status is agent-auto-continue-status for the session env names.
func (e *Engine) Status(env map[string]string, opts OnOptions) (StatusResult, error) {
	h, err := e.HereOf(env, opts)
	if err != nil {
		return StatusResult{}, err
	}
	st := StatusResult{Here: h}
	data, err := os.ReadFile(e.regFile(h.Scope, h.Agent, h.ID))
	if err == nil {
		if r, err := ParseRegistration(h.Scope, string(data)); err == nil {
			st.Registration = &r
		}
	}
	if v, ok := e.blockedSince(h.Scope); ok {
		st.BlockedSince = &v
	}
	return st, nil
}

// Format renders the status as the shell printed it.
func (s StatusResult) Format() string {
	var b strings.Builder
	if s.Registration != nil {
		fmt.Fprintf(&b, "this session: on (%s, targets: %s)\n", s.Here.Scope, strings.Join(s.Registration.Targets, " "))
	} else {
		fmt.Fprintf(&b, "this session: off (%s)\n", s.Here.Scope)
	}
	if s.BlockedSince != nil {
		fmt.Fprintf(&b, "blocked since %s\n", Fmt3339(*s.BlockedSince))
	}
	return b.String()
}

// Fmt3339 is date-unix-to-3339: local time, 2006-01-02 15:04:05-07:00.
func Fmt3339(epoch int64) string {
	return time.Unix(epoch, 0).Format("2006-01-02 15:04:05-07:00")
}

// ListEntry is one registration in a listing.
type ListEntry struct {
	Registration
	Live   bool `json:"live"`
	Marked bool `json:"marked"`
}

// ScopeListing is one scope in a listing.
type ScopeListing struct {
	Scope        string      `json:"scope"`
	Account      string      `json:"account"`
	BlockedSince *int64      `json:"blocked_since,omitempty"`
	Entries      []ListEntry `json:"entries"`
	Pruned       []string    `json:"pruned,omitempty"`
}

// List is agent-auto-continue-list: every registration grouped by scope and
// marked live or dead against one live listing; prune forgets the dead ones.
// When the listing fails, nothing is pruned and every entry reads dead.
func (e *Engine) List(ctx context.Context, prune bool) ([]ScopeListing, error) {
	scopes, err := e.Scopes()
	if err != nil {
		return nil, err
	}
	live, liveErr := e.Lister.Live(ctx)
	var out []ScopeListing
	for _, scope := range scopes {
		sl := ScopeListing{Scope: scope, Account: e.AccountForScope(scope)}
		l := e.scopeLock(scope)
		l.Lock()
		regs, err := e.Registrations(scope)
		if err != nil {
			l.Unlock()
			return nil, err
		}
		for _, r := range regs {
			isL := liveErr == nil && isLive(live, r.ID, r.Transcript)
			if prune && liveErr == nil && !isL {
				if os.Remove(e.regFile(scope, r.Agent, r.ID)) == nil {
					sl.Pruned = append(sl.Pruned, r.Key())
					continue
				}
			}
			sl.Entries = append(sl.Entries, ListEntry{Registration: r, Live: isL, Marked: e.marked(scope, r.Agent, r.ID)})
		}
		if v, ok := e.blockedSince(scope); ok {
			sl.BlockedSince = &v
		}
		l.Unlock()
		out = append(out, sl)
	}
	return out, nil
}

// FormatList renders a listing as the shell printed it (without its colours
// and without the watcher and armed-job lines, which this engine has none of).
func FormatList(ls []ScopeListing) string {
	if len(ls) == 0 {
		return "agent-auto-continue-list: nothing registered\n"
	}
	var b strings.Builder
	for _, s := range ls {
		fmt.Fprintf(&b, "%s\n", s.Scope)
		if s.BlockedSince != nil {
			fmt.Fprintf(&b, "  blocked since %s\n", Fmt3339(*s.BlockedSince))
		}
		for _, en := range s.Entries {
			state := "dead"
			if en.Live {
				state = "live"
			}
			mark := ""
			if en.Marked {
				mark = "  (its own turn hit the limit)"
			}
			fmt.Fprintf(&b, "  %s  %s %s  -> %s%s\n", state, en.Agent, en.ID, strings.Join(en.Targets, " "), mark)
		}
	}
	return b.String()
}

func uniqueInOrder(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
