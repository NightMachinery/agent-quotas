// Package config loads agent-quotas' configuration and resolves its paths.
// Every key is optional; see docs/design.md, "Configuration".
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeProfile is one Claude Code seat.
type ClaudeProfile struct {
	Name string `json:"name"`
	// ConfigDir is CLAUDE_CONFIG_DIR for this seat; empty means the default
	// seat (no variable set, ~/.claude.json, unsuffixed Keychain service).
	ConfigDir string `json:"config_dir"`
	// TokenEnv names an environment variable holding a long-lived token.
	TokenEnv string `json:"token_env,omitempty"`
}

// Claude configures the Claude reader.
type Claude struct {
	Profiles         []ClaudeProfile `json:"profiles"`
	UserAgent        string          `json:"user_agent,omitempty"`
	TokenDir         string          `json:"token_dir,omitempty"`
	MinIntervalS     int             `json:"min_interval_s,omitempty"`
	TimeoutS         int             `json:"timeout_s,omitempty"`
	KeychainTimeoutS int             `json:"keychain_timeout_s,omitempty"`
}

// CodexHome is one CODEX_HOME whose auth snapshots are accounts.
type CodexHome struct {
	Name      string `json:"name"`
	CodexHome string `json:"codex_home"`
}

// Codex configures the Codex reader.
type Codex struct {
	Homes         []CodexHome `json:"homes"`
	AuthBackupDir string      `json:"auth_backup_dir,omitempty"`
	FullPct       float64     `json:"full_pct,omitempty"`
	MinIntervalS  int         `json:"min_interval_s,omitempty"`
	TimeoutS      int         `json:"timeout_s,omitempty"`
	Binary        string      `json:"codex_binary,omitempty"`
}

// AgyAccount is one Antigravity account.
type AgyAccount struct {
	Name      string `json:"name"`
	Label     string `json:"label,omitempty"`
	GeminiDir string `json:"gemini_dir"`
	// CacheFile overrides the statusline cache location.
	CacheFile string `json:"cache_file,omitempty"`
}

// Agy configures the Antigravity reader.
type Agy struct {
	Accounts          []AgyAccount `json:"accounts"`
	StatuslineMaxAgeS *int         `json:"statusline_max_age_s,omitempty"`
	MinIntervalS      int          `json:"min_interval_s,omitempty"`
	TimeoutS          int          `json:"timeout_s,omitempty"`
	Binary            string       `json:"agy_binary,omitempty"`
}

// Poll configures scheduling shared by all pollers.
type Poll struct {
	ForceFloorS int `json:"force_floor_s,omitempty"`
	BackoffMaxS int `json:"backoff_max_s,omitempty"`
	// BlockedGraceS is how long after a blocking window's reset the account
	// is polled again.
	BlockedGraceS int `json:"blocked_grace_s,omitempty"`
}

// Paseo configures the Paseo auto-continue engine.
type Paseo struct {
	Homes            []string `json:"homes"`
	RegistrationsDir string   `json:"registrations_dir,omitempty"`
	SendBehavior     string   `json:"send_behavior,omitempty"`
	Binary           string   `json:"paseo_binary,omitempty"`
}

// Native configures the native auto-continue engine.
type Native struct {
	RegistrationsDir string         `json:"registrations_dir,omitempty"`
	LiveListCommand  []string       `json:"live_list_command,omitempty"`
	Text             string         `json:"text,omitempty"`
	GraceS           int            `json:"grace_s,omitempty"`
	ActivitySlackS   int            `json:"activity_slack_s,omitempty"`
	IdleMinS         map[string]int `json:"idle_min_s,omitempty"`
	LogFile          string         `json:"log_file,omitempty"`
}

// Notify configures notifications.
type Notify struct {
	// Command is an argv; the title and the body are appended. Empty means
	// osascript's display notification.
	Command []string `json:"command,omitempty"`
}

// Config is the whole file.
type Config struct {
	Claude Claude `json:"claude"`
	Codex  Codex  `json:"codex"`
	Agy    Agy    `json:"agy"`
	Poll   Poll   `json:"poll"`
	Paseo  Paseo  `json:"paseo"`
	Native Native `json:"native"`
	Notify Notify `json:"notify"`
	// Env is extra environment for the commands the daemon runs (PATH, ...).
	Env map[string]string `json:"env,omitempty"`
}

// Home returns the user's home directory.
func Home() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// Expand expands a leading ~ to the home directory.
func Expand(p string) string {
	if p == "~" {
		return Home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(Home(), p[2:])
	}
	return p
}

// StateHome is ${XDG_STATE_HOME:-~/.local/state}.
func StateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	return filepath.Join(Home(), ".local", "state")
}

// ConfigHome is ${XDG_CONFIG_HOME:-~/.config}.
func ConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	return filepath.Join(Home(), ".config")
}

// StateDir is the daemon's private state directory.
func StateDir() string {
	if v := os.Getenv("AGENT_QUOTAS_STATE_DIR"); v != "" {
		return v
	}
	return filepath.Join(StateHome(), "agent-quotas")
}

// SocketPath is the daemon's unix socket.
func SocketPath() string {
	if v := os.Getenv("AGENT_QUOTAS_SOCKET"); v != "" {
		return v
	}
	return filepath.Join(StateDir(), "agent-quotas.sock")
}

// Path is the configuration file.
func Path() string {
	if v := os.Getenv("AGENT_QUOTAS_CONFIG"); v != "" {
		return v
	}
	return filepath.Join(ConfigHome(), "agent-quotas", "config.json")
}

// AgentSessionsDir is the shared session registry root of the shell tools.
func AgentSessionsDir() string {
	return filepath.Join(StateHome(), "agent-sessions")
}

// Load reads the file at path, or returns the defaults when it is absent.
func Load(path string) (*Config, error) {
	c := &Config{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.applyDefaults()
	return c, c.validate()
}

func (c *Config) applyDefaults() {
	if len(c.Claude.Profiles) == 0 {
		c.Claude.Profiles = []ClaudeProfile{{Name: "default"}}
	}
	if c.Claude.UserAgent == "" {
		c.Claude.UserAgent = "claude-code/2.1.220"
	}
	if c.Claude.TokenDir == "" {
		c.Claude.TokenDir = "~/.keys"
	}
	if c.Claude.MinIntervalS == 0 {
		c.Claude.MinIntervalS = 300
	}
	if c.Claude.TimeoutS == 0 {
		c.Claude.TimeoutS = 15
	}
	if c.Claude.KeychainTimeoutS == 0 {
		c.Claude.KeychainTimeoutS = 30
	}
	if len(c.Codex.Homes) == 0 {
		c.Codex.Homes = []CodexHome{{Name: "default", CodexHome: "~/.codex"}}
	}
	if c.Codex.AuthBackupDir == "" {
		c.Codex.AuthBackupDir = "~/tmp/.codex-auths"
	}
	if c.Codex.FullPct == 0 {
		c.Codex.FullPct = 100
	}
	if c.Codex.MinIntervalS == 0 {
		c.Codex.MinIntervalS = 300
	}
	if c.Codex.TimeoutS == 0 {
		c.Codex.TimeoutS = 30
	}
	if c.Codex.Binary == "" {
		c.Codex.Binary = "codex"
	}
	if len(c.Agy.Accounts) == 0 {
		c.Agy.Accounts = []AgyAccount{{Name: "m", GeminiDir: "~/.gemini"}}
	}
	if c.Agy.StatuslineMaxAgeS == nil {
		v := -1
		c.Agy.StatuslineMaxAgeS = &v
	}
	if c.Agy.MinIntervalS == 0 {
		c.Agy.MinIntervalS = 60
	}
	if c.Agy.TimeoutS == 0 {
		c.Agy.TimeoutS = 90
	}
	if c.Agy.Binary == "" {
		c.Agy.Binary = "agy"
	}
	if c.Poll.ForceFloorS == 0 {
		c.Poll.ForceFloorS = 5
	}
	if c.Poll.BackoffMaxS == 0 {
		c.Poll.BackoffMaxS = 1800
	}
	if c.Poll.BlockedGraceS == 0 {
		c.Poll.BlockedGraceS = 90
	}
	if len(c.Paseo.Homes) == 0 {
		c.Paseo.Homes = []string{"~/.paseo"}
	}
	if c.Paseo.RegistrationsDir == "" {
		c.Paseo.RegistrationsDir = filepath.Join(AgentSessionsDir(), "auto-continue-paseo")
	}
	if c.Paseo.Binary == "" {
		c.Paseo.Binary = "paseo"
	}
	if c.Native.RegistrationsDir == "" {
		c.Native.RegistrationsDir = filepath.Join(AgentSessionsDir(), "auto-continue")
	}
	if len(c.Native.LiveListCommand) == 0 {
		c.Native.LiveListCommand = []string{"agent_session", "live-all"}
	}
	if c.Native.GraceS == 0 {
		c.Native.GraceS = 120
	}
	if c.Native.ActivitySlackS == 0 {
		c.Native.ActivitySlackS = 300
	}
}

func (c *Config) validate() error {
	seen := map[string]bool{}
	check := func(provider, name string) error {
		if name == "" || strings.ContainsAny(name, ": \t\n") {
			return fmt.Errorf("%s: invalid account name %q", provider, name)
		}
		id := provider + ":" + name
		if seen[id] {
			return fmt.Errorf("duplicate account %s", id)
		}
		seen[id] = true
		return nil
	}
	for _, p := range c.Claude.Profiles {
		if err := check("claude", p.Name); err != nil {
			return err
		}
	}
	for _, h := range c.Codex.Homes {
		if err := check("codex-home", h.Name); err != nil {
			return err
		}
	}
	for _, a := range c.Agy.Accounts {
		if err := check("agy", a.Name); err != nil {
			return err
		}
	}
	return nil
}
