package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// CommandNotifier shows notifications through Command (an argv, to which the
// title and the body are appended) or, when Command is empty, through
// osascript's display notification.
type CommandNotifier struct {
	Runner  procx.Runner
	Command []string
	Env     func() []string
}

// Notify implements Notifier.
func (n CommandNotifier) Notify(ctx context.Context, title, body string) error {
	var argv []string
	if len(n.Command) > 0 {
		argv = append(append(argv, n.Command...), title, body)
	} else {
		argv = []string{"osascript", "-e", "display notification " + appleScriptQuote(body) + " with title " + appleScriptQuote(title)}
	}
	var env []string
	if n.Env != nil {
		env = n.Env()
	}
	_, err := n.Runner.Run(ctx, procx.Cmd{Argv: argv, Env: env, Timeout: 15 * time.Second})
	return err
}

func appleScriptQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// luaQuote makes a Lua string literal of s: quotes, backslashes and control
// bytes are escaped, so nothing in s can end the literal early.
func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TargetKind is a target's kind as spelled in the idle settings:
// claude-bg:<id> is claude_bg.
func TargetKind(target string) string {
	k, _, _ := strings.Cut(target, ":")
	return strings.ReplaceAll(k, "-", "_")
}

// IdleMin is how long the keyboard must have been idle before a resume may
// go to target, in seconds, from cfg.Native.IdleMinS (key "default" for the
// general value). ok is false for a setting that is not a whole number.
func (e *Engine) IdleMin(target string) (v int, ok bool) {
	kind := TargetKind(target)
	m := e.Cfg.Native.IdleMinS
	if x, set := m[kind]; set {
		return x, x >= 0
	}
	if zeroIdleKinds[kind] {
		return 0, true
	}
	if x, set := m["default"]; set {
		return x, x >= 0
	}
	return defaultIdleMinS, true
}

var numberRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// hs runs Lua in Hammerspoon with an empty stdin and a timeout, and returns
// the result with the extension-loading chatter stripped.
func (e *Engine) hs(ctx context.Context, lua string) (string, error) {
	res, err := e.run(ctx, hsTimeout, nil, e.Hammerspoon, "-q", "-c", lua)
	if err != nil {
		return "", err
	}
	var keep []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if !strings.HasPrefix(line, "-- ") {
			keep = append(keep, line)
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n")), nil
}

// IdleSeconds is hs.host.idleTime() in whole seconds; an error means
// "cannot tell".
func (e *Engine) IdleSeconds(ctx context.Context) (int, error) {
	out, err := e.hs(ctx, "return hs.host.idleTime()")
	if err != nil {
		return 0, err
	}
	if !numberRE.MatchString(out) {
		return 0, fmt.Errorf("idle time is not a number: %q", out)
	}
	n, _ := strconv.Atoi(strings.SplitN(out, ".", 2)[0])
	return n, nil
}

// ScreenLocked reports whether the screen is locked.
func (e *Engine) ScreenLocked(ctx context.Context) (bool, error) {
	out, err := e.hs(ctx, `return tostring(hs.caffeinate.sessionProperties()["CGSSessionScreenIsLocked"] == true)`)
	if err != nil {
		return false, err
	}
	switch out {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("unexpected answer: %q", out)
}

// FmtShort is seconds-fmt-short: 0h:10m:0s.
func FmtShort(secs int) string {
	return fmt.Sprintf("%dh:%dm:%ds", secs/3600, secs%3600/60, secs%60)
}

// heldBack is h-agent-usage-continue-held-back: why a resume must not go to
// target now, or "". idle is nil when it could not be read.
func (e *Engine) heldBack(ctx context.Context, target string, idle *int) string {
	min, ok := e.IdleMin(target)
	if !ok {
		return fmt.Sprintf("its idle setting is not a whole number of seconds: %d", min)
	}
	if min > 0 {
		if idle == nil {
			return "could not read the idle time"
		}
		if *idle < min {
			return fmt.Sprintf("you were at the keyboard (%s idle, needs %s)", FmtShort(*idle), FmtShort(min))
		}
	}
	if target == "frontmost" {
		locked, err := e.ScreenLocked(ctx)
		if err != nil {
			// Stricter than the shell, which typed when it could not tell:
			// synthetic keys on a locked screen land in the password field.
			return "could not tell whether the screen is locked"
		}
		if locked {
			return "the screen is locked"
		}
	}
	return ""
}

// FireReport is what one fire did.
type FireReport struct {
	Label       string   `json:"label"`
	Message     string   `json:"message"`
	Resumed     []string `json:"resumed,omitempty"`
	Held        []string `json:"held,omitempty"` // "<target> (<why>)"
	Unreachable []string `json:"unreachable,omitempty"`
	Errors      []string `json:"errors,omitempty"` // "<target>: <why>"
	Report      string   `json:"report"`
}

// Missed is every target not resumed (held back or unreachable).
func (r FireReport) Missed() []string {
	var out []string
	for _, h := range r.Held {
		t, _, _ := strings.Cut(h, " ")
		out = append(out, t)
	}
	return append(out, r.Unreachable...)
}

// Fire is h-agent-usage-arm-fire with action continue: decide per target
// whether the idle gate allows it, deliver the text, then notify with what
// was reached and append one line to the log. The idle time is read once,
// and only when some target's setting needs it.
func (e *Engine) Fire(ctx context.Context, label, msg string, targets []string) FireReport {
	rep := FireReport{Label: label, Message: msg}
	if len(targets) == 0 {
		rep.Report = msg + " -- not resuming: no target was recorded"
		e.notify(ctx, rep.Report)
		e.Log(label + ": notified only, no target recorded")
		return rep
	}
	var idle *int
	for _, t := range targets {
		if m, ok := e.IdleMin(t); ok && m > 0 {
			if n, err := e.IdleSeconds(ctx); err == nil {
				idle = &n
			}
			break
		}
	}
	for _, t := range targets {
		if why := e.heldBack(ctx, t, idle); why != "" {
			rep.Held = append(rep.Held, t+" ("+why+")")
			continue
		}
		send := e.Send
		if e.Deliver != nil {
			send = e.Deliver
		}
		if err := send(ctx, t); err != nil {
			rep.Unreachable = append(rep.Unreachable, t)
			rep.Errors = append(rep.Errors, t+": "+err.Error())
			continue
		}
		rep.Resumed = append(rep.Resumed, t)
	}
	report := msg
	if len(rep.Resumed) > 0 {
		report += " -- resumed " + strings.Join(rep.Resumed, ", ")
	}
	if len(rep.Held) > 0 {
		report += " -- not resuming " + strings.Join(rep.Held, ", ")
	}
	if len(rep.Unreachable) > 0 {
		report += " -- could not reach " + strings.Join(rep.Unreachable, ", ")
	}
	rep.Report = report
	e.notify(ctx, report)
	e.Log(label + ": " + report)
	return rep
}

func (e *Engine) notify(ctx context.Context, body string) {
	if e.Notifier != nil {
		_ = e.Notifier.Notify(ctx, "Auto-continue", body)
	}
}

// Log appends one line to the fire log, as h-agent-usage-arm-log does.
func (e *Engine) Log(msg string) {
	path := e.logFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", e.now().Format("2006-01-02 15:04:05"), msg)
}

var (
	kittyTargetRE = regexp.MustCompile(`^kitty:[0-9]+$`)
	paseoIDRE     = regexp.MustCompile(`^[0-9a-fA-F]{8}-([0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$`)
)

// ErrPaseoTarget is returned for a paseo: target, which belongs to the
// Paseo engine (it re-inspects the agent before sending).
var ErrPaseoTarget = errors.New("paseo targets are delivered by the Paseo engine")

// Send is h-agent-usage-continue-send: deliver the text to one target,
// re-checking at send time that the target still holds a live agent.
func (e *Engine) Send(ctx context.Context, target string) error {
	text := e.text()
	kind, rest, _ := strings.Cut(target, ":")
	switch {
	case kind == "codex" && rest != "":
		// The thread's own CODEX_HOME, which its registered rollout reveals;
		// the daemon's environment carries none.
		home := ""
		if r, ok := e.FindRegistration("codex", rest); ok {
			home = codexHomeOfTranscript(r.Transcript)
		}
		return e.sendCodexIn(ctx, rest, home, text)
	case kind == "paseo":
		if !paseoIDRE.MatchString(rest) {
			return fmt.Errorf("not a Paseo agent id: %s", rest)
		}
		return ErrPaseoTarget
	case kind == "claude-bg" && rest != "":
		return e.sendClaudeBg(ctx, rest, text)
	case target == "frontmost":
		return e.sendFrontmost(ctx, text)
	case kind == "tmux" && rest != "":
		live, err := e.Lister.Live(ctx)
		if err != nil {
			return fmt.Errorf("tmux pane %s: cannot list live sessions: %w", rest, err)
		}
		if !paneHolds(e.TmuxPanes(ctx, live), rest) {
			return fmt.Errorf("tmux pane %s no longer holds a live agent session", rest)
		}
		return e.tmuxSendText(ctx, rest, text)
	case kittyTargetRE.MatchString(target):
		return e.sendKitty(ctx, rest, text)
	}
	return fmt.Errorf("unknown target: %s", target)
}

// sendCodexIn queues the text for a Codex thread with `codex queue`.
func (e *Engine) sendCodexIn(ctx context.Context, thread, codexHome, text string) error {
	env := e.baseEnv()
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	_, err := e.run(ctx, 0, env, e.Cfg.Codex.Binary, "queue", "--thread", thread, "--message", text)
	return err
}

// tmuxSendText is tmux-pane-send-text: refuse a pane that is gone or dead,
// then send the text literally and Enter separately.
func (e *Engine) tmuxSendText(ctx context.Context, pane, text string) error {
	res, err := e.run(ctx, 0, nil, e.Tmux, "display-message", "-p", "-t", pane, "#{pane_dead}")
	dead := ""
	if err == nil {
		dead = strings.TrimSpace(string(res.Stdout))
	}
	if dead == "" {
		return fmt.Errorf("no such tmux pane: %s", pane)
	}
	if dead != "0" {
		return fmt.Errorf("tmux pane has exited, not typing into it: %s", pane)
	}
	if _, err := e.run(ctx, 0, nil, e.Tmux, "send-keys", "-t", pane, "-l", "--", text); err != nil {
		return err
	}
	_, err = e.run(ctx, 0, nil, e.Tmux, "send-keys", "-t", pane, "Enter")
	return err
}

func (e *Engine) sendKitty(ctx context.Context, id, text string) error {
	sock, err := e.KittySocket(map[string]string{"KITTY_LISTEN_ON": os.Getenv("KITTY_LISTEN_ON")})
	if err != nil {
		return err
	}
	wins, err := e.kittyWindows(ctx, sock)
	if err != nil {
		return err
	}
	n, _ := strconv.Atoi(id)
	found := false
	for _, w := range wins {
		if w.ID == n {
			found = true
		}
	}
	// send-text "always succeeds, even if no text was sent to any window",
	// so the window has to be checked for separately.
	if !found {
		return fmt.Errorf("kitty window %s is gone", id)
	}
	_, err = e.run(ctx, 0, nil, e.Kitty, "@", "--to", sock, "send-text", "--match", "id:"+id, text+"\r")
	return err
}

func (e *Engine) sendFrontmost(ctx context.Context, text string) error {
	// Wake the display first: the first synthetic key would otherwise be
	// eaten waking it.
	_, _ = e.hs(ctx, "hs.caffeinate.declareUserActivity() ; return true")
	if err := e.Sleep(ctx, time.Second); err != nil {
		return err
	}
	_, err := e.hs(ctx, "hs.eventtap.keyStrokes("+luaQuote(text)+"); hs.eventtap.keyStroke({}, \"return\");")
	return err
}

// sendClaudeBg is h-claude-code-bg-send-text: attach a scratch tmux session
// to the background session under its own config home, wait for the prompt,
// type, detach with Ctrl+Z and kill the scratch session.
func (e *Engine) sendClaudeBg(ctx context.Context, id, text string) error {
	b, ok := e.ClaudeBgFind(ctx, id)
	if !ok {
		return fmt.Errorf("no running background session %s", id)
	}
	if !validName(b.ShortID) {
		return fmt.Errorf("invalid background session id %q", b.ShortID)
	}
	scratch := "agent-usage-attach-" + b.ShortID
	attach := []string{"env", "-u", "CLAUDE_CONFIG_DIR"}
	if b.ConfigDir != "" {
		attach = append(attach, "CLAUDE_CONFIG_DIR="+b.ConfigDir)
	}
	attach = append(attach, e.Claude, "attach", b.ShortID)

	// tmuxnew replaces a previous session of the same name.
	_, _ = e.run(ctx, 0, nil, e.Tmux, "kill-session", "-t", "="+scratch)
	args := append([]string{e.Tmux, "new-session", "-d", "-s", scratch, "-x", "120", "-y", "30", "-P", "-F", "#{pane_id}"}, attach...)
	res, err := e.run(ctx, 0, nil, args...)
	if err != nil {
		return err
	}
	pane := strings.TrimSpace(string(res.Stdout))
	defer func() {
		_, _ = e.run(context.WithoutCancel(ctx), 0, nil, e.Tmux, "kill-session", "-t", "="+scratch)
	}()
	if pane == "" {
		return fmt.Errorf("%s: scratch tmux session has no pane", b.ShortID)
	}

	wait := e.BgAttachWait
	ready := false
	for waited := time.Duration(0); waited < wait; waited += time.Second {
		out, err := e.run(ctx, 0, nil, e.Tmux, "capture-pane", "-p", "-t", pane)
		if err == nil && strings.Contains(string(out.Stdout), "❯") {
			ready = true
			break
		}
		if err := e.Sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	var sendErr error
	if !ready {
		sendErr = fmt.Errorf("%s: no prompt after %s of attaching, not typing", b.ShortID, wait)
	} else {
		sendErr = e.tmuxSendText(ctx, pane, text)
		_ = e.Sleep(ctx, time.Second)
	}
	_, _ = e.run(ctx, 0, nil, e.Tmux, "send-keys", "-t", pane, "C-z")
	_ = e.Sleep(ctx, time.Second)
	return sendErr
}
