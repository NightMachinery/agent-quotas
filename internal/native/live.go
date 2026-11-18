package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/NightMachinery/agent-quotas/internal/config"
)

// CommandLister runs cfg.Native.LiveListCommand (agent_session live-all).
// live-all refuses to run without <agent>=<root> specs, so when the command
// names none, the specs are built from the configuration: every Claude
// profile's projects directory, every Codex home's sessions directory and
// every Antigravity account's brain directory that exists.
type CommandLister struct {
	Engine *Engine
}

// Roots returns the <agent>=<root> specs for the configured accounts.
func (e *Engine) Roots() []string {
	var specs []string
	seen := map[string]bool{}
	add := func(agent, dir string) {
		dir = filepath.Clean(dir)
		spec := agent + "=" + dir
		if seen[spec] {
			return
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return
		}
		seen[spec] = true
		specs = append(specs, spec)
	}
	for _, p := range e.Cfg.Claude.Profiles {
		d := config.Expand(p.ConfigDir)
		if d == "" {
			d = filepath.Join(config.Home(), ".claude")
		}
		add("claude", filepath.Join(d, "projects"))
	}
	for _, h := range e.Cfg.Codex.Homes {
		add("codex", filepath.Join(config.Expand(h.CodexHome), "sessions"))
	}
	for _, a := range e.Cfg.Agy.Accounts {
		add("agy", filepath.Join(config.Expand(a.GeminiDir), "antigravity-cli", "brain"))
	}
	return specs
}

// Live runs the listing once and parses its rows.
func (l *CommandLister) Live(ctx context.Context) ([]LiveRow, error) {
	e := l.Engine
	argv := append([]string(nil), e.Cfg.Native.LiveListCommand...)
	if len(argv) == 0 {
		return nil, errors.New("no live_list_command configured")
	}
	hasSpec := false
	for _, a := range argv[1:] {
		if strings.Contains(a, "=") {
			hasSpec = true
		}
	}
	if !hasSpec && argv[len(argv)-1] == "live-all" {
		specs := e.Roots()
		if len(specs) == 0 {
			return nil, nil
		}
		argv = append(argv, specs...)
	}
	res, err := e.run(ctx, 0, nil, argv...)
	if err != nil {
		return nil, fmt.Errorf("live listing: %w", err)
	}
	return ParseLive(string(res.Stdout)), nil
}

// ParseLive parses live-all's TSV rows.
func ParseLive(out string) []LiveRow {
	var rows []LiveRow
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		for len(f) < 8 {
			f = append(f, "")
		}
		rows = append(rows, LiveRow{PID: f[0], ID: f[1], Name: f[2], Cwd: f[3], Transcript: f[4], Tmux: f[5], Status: f[6], Kind: f[7]})
	}
	return rows
}

// isLive is h-agent-auto-continue-live-p: the id (column 2) or the
// transcript (column 5) matches.
func isLive(rows []LiveRow, id, transcript string) bool {
	for _, r := range rows {
		if id != "" && r.ID == id {
			return true
		}
		if transcript != "" && r.Transcript == transcript {
			return true
		}
	}
	return false
}

// excludeRE is agent_session_exclude_pcres' default: throwaway sessions no
// menu offers (one opened only to refresh a token), matched on the session's
// name and on its tmux session.
var excludeRE = regexp.MustCompile(`(?:^|-)refresh-token$`)

func offerable(rows []LiveRow) []LiveRow {
	var out []LiveRow
	for _, r := range rows {
		drop := false
		for _, f := range []string{r.Name, r.Tmux} {
			if f != "" && f != "-" && excludeRE.MatchString(f) {
				drop = true
			}
		}
		if !drop {
			out = append(out, r)
		}
	}
	return out
}

// underRoots reports whether transcript sits under one of the roots
// (<agent>=<dir> specs); with no roots, everything passes.
func underRoots(transcript string, specs []string) bool {
	if len(specs) == 0 {
		return true
	}
	for _, s := range specs {
		_, dir, _ := strings.Cut(s, "=")
		if strings.HasPrefix(transcript, strings.TrimRight(dir, "/")+"/") {
			return true
		}
	}
	return false
}

// psTable is pid -> ppid from one `ps -axo pid=,ppid=`.
func (e *Engine) psTable(ctx context.Context) map[string]string {
	res, err := e.run(ctx, 0, nil, e.PS, "-axo", "pid=,ppid=")
	ppid := map[string]string{}
	if err != nil {
		return ppid
	}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			ppid[f[0]] = f[1]
		}
	}
	return ppid
}

// PaneRow is one live agent session sitting in a tmux pane.
type PaneRow struct {
	Pane       string
	Transcript string
}

// TmuxPanes is h-agent-session-tmux-panes: one row per live agent session
// in a live tmux pane, found by walking the agent's pid up its parents until
// one is some pane's own process. Dead panes are skipped.
func (e *Engine) TmuxPanes(ctx context.Context, live []LiveRow) []PaneRow {
	live = offerable(live)
	if len(live) == 0 {
		return nil
	}
	res, err := e.run(ctx, 0, nil, e.Tmux, "list-panes", "-a", "-F", "#{pane_id}\t#{pane_pid}\t#{pane_dead}")
	if err != nil {
		return nil
	}
	paneOf := map[string]string{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 || f[0] == "" || f[1] == "" || f[2] != "0" {
			continue
		}
		paneOf[f[1]] = f[0]
	}
	if len(paneOf) == 0 {
		return nil
	}
	ppid := e.psTable(ctx)
	roots := e.Roots()
	var out []PaneRow
	for _, r := range live {
		t := r.Transcript
		if t == "" {
			continue
		}
		if _, err := os.Stat(t); err != nil {
			continue
		}
		if !underRoots(t, roots) {
			continue
		}
		cur := r.PID
		for hops := 0; hops < 64; hops++ {
			n, err := strconv.Atoi(cur)
			if err != nil || n <= 1 {
				break
			}
			if p, ok := paneOf[cur]; ok {
				out = append(out, PaneRow{Pane: p, Transcript: t})
				break
			}
			cur = ppid[cur]
		}
	}
	return out
}

func paneHolds(rows []PaneRow, pane string) bool {
	for _, r := range rows {
		if r.Pane == pane {
			return true
		}
	}
	return false
}

// KittySocket is h-agent-session-kitty-socket: KITTY_LISTEN_ON when it names
// a socket that exists, else the one live kitty socket matching the glob.
func (e *Engine) KittySocket(env map[string]string) (string, error) {
	if v := env["KITTY_LISTEN_ON"]; strings.HasPrefix(v, "unix:") {
		if _, err := os.Stat(strings.TrimPrefix(v, "unix:")); err == nil {
			return v, nil
		}
	}
	socks, _ := filepath.Glob(e.KittySocketGlob)
	var live []string
	for _, s := range socks {
		if pid := kittySocketPID(s); pid > 0 && (e.ProcAlive == nil || e.ProcAlive(pid)) {
			live = append(live, s)
		}
	}
	switch len(live) {
	case 1:
		return "unix:" + live[0], nil
	case 0:
		return "", errors.New("no live kitty remote-control socket")
	}
	return "", fmt.Errorf("several live kitty sockets: %d", len(live))
}

// kittySocketPID reads kitty's pid out of a socket name kitty-<pid>.sock.
func kittySocketPID(sock string) int {
	s := strings.TrimPrefix(sock, "unix:")
	s = s[strings.LastIndex(s, "-")+1:]
	s = strings.TrimSuffix(s, ".sock")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

type kittyWindow struct {
	ID    int
	Title string
	PIDs  []string
	Cmds  [][]string
}

type kittyLS []struct {
	Tabs []struct {
		Windows []struct {
			ID                  int    `json:"id"`
			Title               string `json:"title"`
			ForegroundProcesses []struct {
				PID     int      `json:"pid"`
				Cmdline []string `json:"cmdline"`
			} `json:"foreground_processes"`
		} `json:"windows"`
	} `json:"tabs"`
}

func (e *Engine) kittyWindows(ctx context.Context, sock string) ([]kittyWindow, error) {
	res, err := e.run(ctx, 0, nil, e.Kitty, "@", "--to", sock, "ls")
	if err != nil {
		return nil, err
	}
	var ls kittyLS
	if err := json.Unmarshal(res.Stdout, &ls); err != nil {
		return nil, fmt.Errorf("kitty @ ls: %w", err)
	}
	var out []kittyWindow
	for _, inst := range ls {
		for _, t := range inst.Tabs {
			for _, w := range t.Windows {
				kw := kittyWindow{ID: w.ID, Title: w.Title}
				for _, p := range w.ForegroundProcesses {
					kw.PIDs = append(kw.PIDs, strconv.Itoa(p.PID))
					kw.Cmds = append(kw.Cmds, p.Cmdline)
				}
				out = append(out, kw)
			}
		}
	}
	return out, nil
}

// tmuxTable runs a tmux listing of "<key>\t<rest>" rows.
func (e *Engine) tmuxTable(ctx context.Context, args ...string) map[string]string {
	res, err := e.run(ctx, 0, nil, append([]string{e.Tmux}, args...)...)
	out := map[string]string{}
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		k, v, ok := strings.Cut(line, "\t")
		if ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// KittyPair is one kitty window showing an agent session.
type KittyPair struct {
	Window     int
	Transcript string
}

// KittyPairs is h-agent-session-live-pairs, through the resolution order of
// h-agent-session-of-kitty-window: (0) the hooks' @agent_session record on
// the tmux session a window's client is attached to, (1) a foreground pid of
// the window (or a child of a decset-rewrite wrapper) is a live session's
// pid, (2) the client's tmux session is a live row's tmux column, (4) the
// window registry. The title heuristic (3) is not ported.
func (e *Engine) KittyPairs(ctx context.Context, env map[string]string, live []LiveRow) ([]KittyPair, error) {
	sock, err := e.KittySocket(env)
	if err != nil {
		return nil, err
	}
	wins, err := e.kittyWindows(ctx, sock)
	if err != nil {
		return nil, err
	}
	clients := e.tmuxTable(ctx, "list-clients", "-F", "#{client_pid}\t#{client_session}")
	identities := e.tmuxTable(ctx, "list-sessions", "-F", "#{session_name}\t#{@agent_session}")
	var ppid map[string]string
	kpid := kittySocketPID(sock)
	exists := func(p string) bool {
		if p == "" {
			return false
		}
		_, err := os.Stat(p)
		return err == nil
	}
	var out []KittyPair
	for _, w := range wins {
		t := ""
		var tsessions []string
		// 0. The hooks' record on the attached tmux session.
		for _, pid := range w.PIDs {
			name, ok := clients[pid]
			if !ok || name == "" {
				continue
			}
			tsessions = append(tsessions, name)
			f := strings.Split(identities[name], "\t")
			if len(f) >= 3 && exists(f[2]) {
				t = f[2]
				break
			}
		}
		// 1. The agent in the window itself.
		if t == "" {
			pids := append([]string(nil), w.PIDs...)
			for i, cmd := range w.Cmds {
				if len(cmd) > 0 && filepath.Base(cmd[0]) == "decset-rewrite" {
					if ppid == nil {
						ppid = e.psTable(ctx)
					}
					for kid, parent := range ppid {
						if parent == w.PIDs[i] {
							pids = append(pids, kid)
						}
					}
				}
			}
			hits := uniqueTranscripts(live, func(r LiveRow) bool { return contains(pids, r.PID) }, exists)
			if len(hits) > 1 {
				continue
			}
			if len(hits) == 1 {
				t = hits[0]
			}
		}
		// 2. A tmux client whose session the hooks never recorded.
		if t == "" && len(tsessions) > 0 {
			hits := uniqueTranscripts(live, func(r LiveRow) bool { return contains(tsessions, r.Tmux) }, exists)
			if len(hits) > 1 {
				continue
			}
			if len(hits) == 1 {
				t = hits[0]
			}
		}
		// 4. The registry the hooks keep as insurance.
		if t == "" && kpid > 0 {
			key := fmt.Sprintf("%d-%d", kpid, w.ID)
			for _, dir := range []string{
				config.AgentSessionsDir(),
				filepath.Join(config.StateHome(), "claude-code-sessions"),
			} {
				data, err := os.ReadFile(filepath.Join(dir, key))
				if err != nil {
					continue
				}
				if v := strings.TrimSpace(string(data)); exists(v) {
					t = v
					break
				}
			}
		}
		if t != "" {
			out = append(out, KittyPair{Window: w.ID, Transcript: t})
		}
	}
	return out, nil
}

func uniqueTranscripts(live []LiveRow, match func(LiveRow) bool, exists func(string) bool) []string {
	seen := map[string]bool{}
	var hits []string
	for _, r := range live {
		if match(r) && exists(r.Transcript) && !seen[r.Transcript] {
			seen[r.Transcript] = true
			hits = append(hits, r.Transcript)
		}
	}
	return hits
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// BgSession is one running background Claude Code session.
type BgSession struct {
	ConfigDir string // empty for the default seat (CLAUDE_CONFIG_DIR unset)
	ShortID   string
	State     string
	SessionID string
}

// ClaudeBgList is h-claude-code-bg-list: `claude agents --json` per
// configured seat, keeping the running background sessions.
func (e *Engine) ClaudeBgList(ctx context.Context) []BgSession {
	var out []BgSession
	seen := map[string]bool{}
	defaultDir := filepath.Join(config.Home(), ".claude")
	for _, p := range e.Cfg.Claude.Profiles {
		dir := config.Expand(p.ConfigDir)
		if dir == defaultDir {
			dir = ""
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		res, err := e.run(ctx, 0, e.claudeEnv(dir), e.Claude, "agents", "--json")
		if err != nil {
			continue
		}
		var agents []struct {
			Kind      string  `json:"kind"`
			PID       *int    `json:"pid"`
			ID        string  `json:"id"`
			State     *string `json:"state"`
			SessionID *string `json:"sessionId"`
		}
		if json.Unmarshal(res.Stdout, &agents) != nil {
			continue
		}
		for _, a := range agents {
			if a.Kind != "background" || a.PID == nil || a.SessionID == nil {
				continue
			}
			st := "-"
			if a.State != nil {
				st = *a.State
			}
			out = append(out, BgSession{ConfigDir: dir, ShortID: a.ID, State: st, SessionID: *a.SessionID})
		}
	}
	return out
}

func (e *Engine) claudeEnv(configDir string) []string {
	env := e.baseEnv()
	if configDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+configDir)
	}
	return env
}

// ClaudeBgFind is h-claude-code-bg-find.
func (e *Engine) ClaudeBgFind(ctx context.Context, id string) (BgSession, bool) {
	for _, b := range e.ClaudeBgList(ctx) {
		if b.SessionID == id {
			return b, true
		}
	}
	return BgSession{}, false
}

func sortedUnique(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
