package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/claude"
	"github.com/NightMachinery/agent-quotas/internal/codex"
	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
)

// colorFlag resolves an auto|always|never colour knob for stdout.
func colorFlag(mode string) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	switch mode {
	case "always", "y", "on":
		return true
	case "never", "n", "off":
		return false
	}
	return stdoutIsTerminal()
}

func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// trueColor resolves an on|off|auto true-colour knob as the old tools did:
// kitty first, then COLORTERM.
func trueColor(mode string) bool {
	switch mode {
	case "on", "y", "always":
		return true
	case "off", "n", "never":
		return false
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" || strings.Contains(os.Getenv("TERM"), "kitty") {
		return true
	}
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	return ct == "truecolor" || ct == "24bit"
}

func terminalColumns(def int) int {
	var n int
	if _, err := fmt.Sscan(os.Getenv("COLUMNS"), &n); err == nil && n > 0 {
		return n
	}
	return def
}

// providerSnapshots reads every account of one provider (or the named
// accounts) from the daemon or directly.
func providerSnapshots(ctx context.Context, provider string, selectors []string, refresh, direct bool) ([]model.Snapshot, int) {
	if len(selectors) == 0 {
		selectors = []string{provider}
	}
	doc, code := readDocument(ctx, selectors, refresh, direct)
	if code != exitOK {
		return nil, code
	}
	return doc.Accounts, exitOK
}

func natives(snaps []model.Snapshot) []json.RawMessage {
	var rows []json.RawMessage
	for _, s := range snaps {
		if len(s.Native) > 0 {
			rows = append(rows, s.Native)
		}
	}
	return rows
}

// ---- codex-status ----

func cmdCodexStatus(ctx context.Context, args []string) int {
	fs := newFlags("codex-status")
	rf := addReadFlags(fs)
	all := fs.Bool("all", true, "every auth snapshot (--all=false: the active one only)")
	noAll := fs.Bool("no-all", false, "only the active auth")
	fullPct := fs.Float64("full-pct", 0, "utilization at which a window counts as spent (default: config)")
	colorMode := fs.String("color", "auto", "auto|always|never")
	tc := fs.String("true-color", "auto", "on|off|auto")
	darkMode := fs.String("dark-mode", "on", "on|off (light themes when off)")
	darkTheme := fs.String("dark-theme", "", "neon|ember|ocean")
	lightTheme := fs.String("light-theme", "", "day|paper|mint")
	if _, err := parseInterspersed(fs, args); err != nil {
		return exitUsage
	}
	if *noAll {
		*all = false
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: config: %v\n", err)
		return exitFail
	}
	pct := *fullPct
	if pct == 0 {
		pct = cfg.Codex.FullPct
	}
	snaps, code := providerSnapshots(ctx, model.Codex, nil, *rf.refresh || deus(), *rf.direct)
	if code != exitOK {
		return code
	}
	// The default home only, as codex-status reads ~/.codex.
	var chosen []model.Snapshot
	for _, s := range snaps {
		if !strings.Contains(s.Name, "/") {
			chosen = append(chosen, s)
		}
	}
	if !*all {
		var active []model.Snapshot
		for _, s := range chosen {
			if s.Active != nil && *s.Active {
				active = append(active, s)
			}
		}
		if len(active) == 0 && len(chosen) > 0 {
			active = chosen[:1]
		}
		chosen = active
	}
	rows := natives(chosen)
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "codex-status: no Codex auth found")
		return exitFail
	}
	failed := false
	for _, r := range rows {
		var ok struct {
			OK *bool `json:"ok"`
		}
		if json.Unmarshal(r, &ok) == nil && ok.OK != nil && !*ok.OK {
			failed = true
		}
	}
	if *rf.json {
		data, err := codex.LegacyDocument(rows, *all, time.Now(), pct)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codex-status: %v\n", err)
			return exitFail
		}
		os.Stdout.Write(data)
		if !bytes.HasSuffix(data, []byte("\n")) {
			fmt.Println()
		}
	} else {
		home := codexDefaultHome(cfg)
		opts := codex.RenderOptions{Color: colorFlag(*colorMode), All: *all, FullPct: pct,
			ActivePath: filepath.Join(home, "auth.json"), Now: time.Now(),
			DarkTheme: *darkTheme, LightTheme: *lightTheme, Light: *darkMode == "off"}
		opts.TrueColor = opts.Color && trueColor(*tc)
		if err := codex.RenderHuman(os.Stdout, rows, opts); err != nil {
			fmt.Fprintf(os.Stderr, "codex-status: %v\n", err)
			return exitFail
		}
	}
	if failed {
		return exitFail
	}
	return exitOK
}

func codexDefaultHome(cfg *config.Config) string {
	for _, h := range cfg.Codex.Homes {
		if h.Name == "default" {
			return config.Expand(h.CodexHome)
		}
	}
	return config.Expand("~/.codex")
}

var _ = flag.ErrHelp

// ---- claude-code-usage ----

func cmdClaudeCodeUsage(ctx context.Context, args []string) int {
	fs := newFlags("claude-code-usage")
	rf := addReadFlags(fs)
	all := fs.Bool("all", false, "every configured profile")
	label := fs.String("profile-label", os.Getenv("claude_code_usage_profile"), "the profile to report (default: default)")
	var profiles multiFlag
	fs.Var(&profiles, "profile", "NAME or NAME=CONFIG_DIR (repeatable; the config dir is ignored, the daemon's config decides)")
	configDir := fs.String("config-dir", "", "select the profile whose config dir this is")
	cloud := fs.Bool("cloud-credit", false, "print only each profile's cloud credit line")
	colorMode := fs.String("color", "auto", "auto|always|never")
	tc := fs.String("true-color", "auto", "on|off|auto")
	darkMode := fs.String("dark-mode", "on", "on|off")
	_ = fs.Bool("no-relogin", true, "accepted for compatibility; the daemon never relogs in")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: config: %v\n", err)
		return exitFail
	}
	var names []string
	for _, p := range append([]string(profiles), pos...) {
		name, _, _ := strings.Cut(p, "=")
		names = append(names, name)
	}
	if *configDir != "" {
		want := canonical(*configDir)
		for _, p := range cfg.Claude.Profiles {
			if p.ConfigDir != "" && canonical(p.ConfigDir) == want {
				names = append(names, p.Name)
			}
		}
		if len(names) == 0 {
			fmt.Fprintf(os.Stderr, "claude-code-usage: no configured profile has config dir %s\n", *configDir)
			return exitFail
		}
	}
	if len(names) == 0 && !*all {
		n := *label
		if n == "" {
			n = "default"
		}
		names = []string{n}
	}
	var selectors []string
	for _, n := range names {
		selectors = append(selectors, model.ID(model.Claude, n))
	}
	refresh := *rf.refresh || deus()
	snaps, code := providerSnapshots(ctx, model.Claude, selectors, refresh, *rf.direct)
	if code != exitOK {
		return code
	}
	var rows []json.RawMessage
	for _, s := range snaps {
		rows = append(rows, claude.LegacyRow(s, !refresh && !*rf.direct))
	}
	multi := *all || len(rows) > 1
	if *rf.json {
		os.Stdout.Write(claude.LegacyJSON(rows, multi))
		return claude.LegacyExitCode(rows)
	}
	opts := claude.RenderOptions{Color: colorFlag(*colorMode), Width: terminalColumns(100), Now: time.Now(), Light: *darkMode == "off"}
	opts.TrueColor = opts.Color && trueColor(*tc)
	render := claude.RenderHuman
	if *cloud {
		render = claude.RenderCloudCredit
	}
	if err := render(os.Stdout, rows, opts); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code-usage: %v\n", err)
		return exitFail
	}
	return claude.LegacyExitCode(rows)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
