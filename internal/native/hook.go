package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// HookResult is what a hook kick did.
type HookResult struct {
	// Scope is the scope to check now, or "" when nothing should run (the
	// session is not registered, or the scope was kicked too recently).
	Scope string `json:"scope,omitempty"`
	// Marked is true when the session's own turn ended on an error and its
	// mark was written.
	Marked bool `json:"marked,omitempty"`
	// Debounced is true when the kick came within KickMin of the last one.
	Debounced bool `json:"debounced,omitempty"`
}

// Hook is agent-auto-continue-hook, the body of Claude Code's StopFailure
// and Antigravity's Stop hooks: for a registered session, mark it when its
// own turn ended on an error, and return its scope so the daemon refreshes
// that account and runs Check, debounced per scope on .kicked. pane is the
// hook's TMUX_PANE; like the shell, it is unused (the target was fixed at
// registration). A payload naming no registered session is a no-op.
func (e *Engine) Hook(ctx context.Context, agent, pane string, payload []byte) (HookResult, error) {
	var res HookResult
	var p struct {
		SessionID         string          `json:"session_id"`
		AgentID           json.RawMessage `json:"agent_id"`
		ConversationID    string          `json:"conversationId"`
		TerminationReason string          `json:"terminationReason"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &p) != nil {
		return res, nil
	}
	id, failed := "", false
	switch agent {
	case "claude":
		// StopFailure fires only on an error turn, and the hook line's
		// matcher narrows it to rate_limit.
		id, failed = p.SessionID, true
	case "codex":
		// A subagent's payload carries agent_id; its session_id is the root
		// thread's, which registered itself.
		if len(p.AgentID) == 0 || string(p.AgentID) == "null" {
			id = p.SessionID
		}
	case "agy":
		id, failed = p.ConversationID, p.TerminationReason == "error"
	}
	if id == "" {
		return res, nil
	}
	r, ok := e.FindRegistration(agent, id)
	if !ok {
		return res, nil
	}
	l := e.scopeLock(r.Scope)
	l.Lock()
	defer l.Unlock()

	if failed {
		if err := touch(e.markFile(r.Scope, agent, id), e.now()); err == nil {
			res.Marked = true
		}
	}
	stamp := filepath.Join(e.scopeDir(r.Scope), kickedFile)
	if st, err := os.Stat(stamp); err == nil && e.now().Sub(st.ModTime()) < e.KickMin {
		res.Debounced = true
		return res, nil
	}
	_ = touch(stamp, e.now())
	res.Scope = r.Scope
	return res, nil
}

func touch(path string, now time.Time) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	f.Close()
	return os.Chtimes(path, now, now)
}
