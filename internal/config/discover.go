package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Discover proposes a configuration from what exists on this machine, for
// `agent-quotas config init`. getenv supplies the calling shell's
// environment (AGY_ACCOUNTS and friends). Paths are written with ~ so the
// file reads the same on every machine.
func Discover(getenv func(string) string) *Config {
	home := Home()
	tilde := func(p string) string {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
		return p
	}
	c := &Config{}
	c.Claude.Profiles = []ClaudeProfile{{Name: "default"}}
	if matches, _ := filepath.Glob(filepath.Join(home, ".claude-*")); len(matches) > 0 {
		sort.Strings(matches)
		for _, dir := range matches {
			if info, err := os.Stat(filepath.Join(dir, ".claude.json")); err != nil || info.IsDir() {
				continue
			}
			name := strings.TrimPrefix(filepath.Base(dir), ".claude-")
			if name == "" || strings.ContainsAny(name, ": \t") {
				continue
			}
			c.Claude.Profiles = append(c.Claude.Profiles, ClaudeProfile{Name: name, ConfigDir: tilde(dir)})
		}
	}
	c.Codex.Homes = []CodexHome{{Name: "default", CodexHome: "~/.codex"}}

	names := strings.Fields(getenv("AGY_ACCOUNTS"))
	if len(names) == 0 {
		names = []string{"m"}
		if entries, err := os.ReadDir(filepath.Join(home, ".agy-accounts")); err == nil {
			for _, e := range entries {
				if e.IsDir() && e.Name() != "m" {
					names = append(names, e.Name())
				}
			}
		}
	}
	for _, name := range names {
		upper := strings.ToUpper(name)
		dir := getenv("AGY_ACCOUNT_DIR_" + upper)
		if dir == "" {
			if name == "m" {
				dir = filepath.Join(home, ".gemini")
			} else {
				dir = filepath.Join(home, ".agy-accounts", name)
			}
		}
		acct := AgyAccount{Name: name, Label: getenv("AGY_ACCOUNT_LABEL_" + upper), GeminiDir: tilde(dir)}
		if (name == "m" || name == "acc1") && getenv("AGY_MAIN_STATUS_CACHE_FILE") != "" {
			acct.CacheFile = tilde(getenv("AGY_MAIN_STATUS_CACHE_FILE"))
		}
		c.Agy.Accounts = append(c.Agy.Accounts, acct)
	}
	c.Paseo.Homes = []string{"~/.paseo"}
	return c
}
