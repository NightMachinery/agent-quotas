package native

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/NightMachinery/agent-quotas/internal/config"
)

// ErrPaseoAgent is returned for an environment inside a Paseo agent
// (PASEO_AGENT_ID set): that registration belongs to the Paseo engine.
var ErrPaseoAgent = errors.New("inside a Paseo agent: use the Paseo engine")

// ErrNotAgent is returned when the environment names no agent session.
var ErrNotAgent = errors.New("not inside an agent session")

// AgentOf says which agent an environment belongs to, by the same markers
// as ai-agent-name: claude, codex or agy.
func AgentOf(env map[string]string) (string, error) {
	ai := env["AI_AGENT"]
	switch {
	case env["CLAUDECODE"] == "1" || strings.HasPrefix(ai, "claude"):
		return "claude", nil
	case env["CODEX_SANDBOX"] != "" || env["CODEX_THREAD_ID"] != "" || env["CODEX_SESSION_ID"] != "" || strings.HasPrefix(ai, "codex"):
		return "codex", nil
	case env["ANTIGRAVITY_AGENT"] != "" || env["ANTIGRAVITY_TRAJECTORY_ID"] != "" || env["ANTIGRAVITY_CONVERSATION_ID"] != "" ||
		strings.HasPrefix(ai, "antigravity") || strings.HasPrefix(ai, "agy"):
		return "agy", nil
	}
	return "", ErrNotAgent
}

// SessionIDOf is the session id the agent exports into its shells.
func SessionIDOf(agent string, env map[string]string) (string, error) {
	var id, name string
	switch agent {
	case "claude":
		id, name = env["CLAUDE_CODE_SESSION_ID"], "CLAUDE_CODE_SESSION_ID"
	case "codex":
		id, name = env["CODEX_THREAD_ID"], "CODEX_THREAD_ID"
		if id == "" {
			id = env["CODEX_SESSION_ID"]
		}
	case "agy":
		id, name = env["ANTIGRAVITY_CONVERSATION_ID"], "ANTIGRAVITY_CONVERSATION_ID"
	default:
		return "", fmt.Errorf("unknown agent: %s", agent)
	}
	if id == "" {
		return "", fmt.Errorf("%s session id unknown (%s is unset)", agent, name)
	}
	if !validName(id) {
		return "", fmt.Errorf("invalid session id: %q", id)
	}
	return id, nil
}

func envHome(env map[string]string) string {
	if h := env["HOME"]; h != "" {
		return h
	}
	return config.Home()
}

// ClaudeProfileOf is claude-code-profile-current: CLAUDE_CONFIG_DIR matched
// against the configured profiles. Unset means the profile with no config
// dir (or one whose dir is ~/.claude); an unregistered dir is named after its
// basename without the leading dot.
func (e *Engine) ClaudeProfileOf(env map[string]string) string {
	dir := strings.TrimRight(env["CLAUDE_CONFIG_DIR"], "/")
	home := envHome(env)
	defaultDir := filepath.Join(home, ".claude")
	for _, p := range e.Cfg.Claude.Profiles {
		pd := strings.TrimRight(config.Expand(p.ConfigDir), "/")
		if pd == dir {
			return p.Name
		}
		if pd == "" && dir == defaultDir {
			return p.Name
		}
		if pd == defaultDir && dir == "" {
			return p.Name
		}
	}
	if dir == "" {
		return "default"
	}
	return strings.TrimPrefix(filepath.Base(dir), ".")
}

var authSnapshotRE = regexp.MustCompile(`^auth[._-].*\.json$`)

// CodexActiveAlias is h-codex-status-active-alias for one CODEX_HOME: the
// alias of the auth_<alias>.json whose bytes match auth.json, else "auth".
// ok is false when auth.json does not exist (the shell then falls back to
// "auth" too).
func CodexActiveAlias(codexHome string) (alias string, ok bool) {
	bare, err := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	if err != nil {
		return "auth", false
	}
	ents, err := os.ReadDir(codexHome)
	if err != nil {
		return "auth", true
	}
	var names []string
	for _, ent := range ents {
		if ent.Type().IsRegular() && authSnapshotRE.MatchString(ent.Name()) {
			names = append(names, ent.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(codexHome, n))
		if err == nil && bytes.Equal(data, bare) {
			return strings.TrimSuffix(n, ".json")[len("auth_"):], true
		}
	}
	return "auth", true
}

func codexHomeOf(env map[string]string) string {
	if h := env["CODEX_HOME"]; h != "" {
		return h
	}
	return filepath.Join(envHome(env), ".codex")
}

// ScopeOf is h-agent-auto-continue-scope: the account a session of agent
// in env shares its limit with.
func (e *Engine) ScopeOf(agent string, env map[string]string) (string, error) {
	switch agent {
	case "claude":
		return "claude-" + e.ClaudeProfileOf(env), nil
	case "codex":
		alias, _ := CodexActiveAlias(codexHomeOf(env))
		return "codex-" + alias, nil
	case "agy":
		return "agy", nil
	}
	return "", fmt.Errorf("unknown agent: %s", agent)
}

// ResolveTranscript finds a session's transcript from its id, as each
// adapter's `resolve` verb does; empty when there is none yet.
func ResolveTranscript(agent, id string, env map[string]string) string {
	if !validName(id) {
		return ""
	}
	home := envHome(env)
	switch agent {
	case "claude":
		dir := env["CLAUDE_CONFIG_DIR"]
		if dir == "" {
			dir = filepath.Join(home, ".claude")
		}
		hits, _ := filepath.Glob(filepath.Join(dir, "projects", "*", id+".jsonl"))
		if len(hits) > 0 {
			sort.Strings(hits)
			return hits[0]
		}
	case "codex":
		hits, _ := filepath.Glob(filepath.Join(codexHomeOf(env), "sessions", "*", "*", "*", "rollout-*-"+id+"*.jsonl"))
		if len(hits) > 0 {
			sort.Strings(hits)
			return hits[len(hits)-1]
		}
	case "agy":
		gemini := env["ANTIGRAVITY_GEMINI_DIR"]
		if gemini == "" {
			gemini = filepath.Join(home, ".gemini")
		}
		logs := filepath.Join(gemini, "antigravity-cli", "brain", id, ".system_generated", "logs")
		for _, n := range []string{"transcript_full.jsonl", "transcript.jsonl"} {
			p := filepath.Join(logs, n)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// codexHomeOfTranscript is the CODEX_HOME a rollout sits under, or "".
func codexHomeOfTranscript(transcript string) string {
	i := strings.Index(transcript, "/sessions/")
	if i <= 0 {
		return ""
	}
	return transcript[:i]
}
