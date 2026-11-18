package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hsFake answers the Hammerspoon calls: idle time, lock state, typing.
func hsFake(idle string, locked string) func(argv []string) (string, error) {
	return func(argv []string) (string, error) {
		if argv[0] != "hs" {
			return "", errors.New("unexpected: " + strings.Join(argv, " "))
		}
		lua := argv[len(argv)-1]
		switch {
		case strings.Contains(lua, "idleTime"):
			if idle == "fail" {
				return "", errors.New("hs down")
			}
			return "-- Loading extension: host\n" + idle + "\n", nil
		case strings.Contains(lua, "CGSSessionScreenIsLocked"):
			if locked == "fail" {
				return "", errors.New("hs down")
			}
			return locked + "\n", nil
		}
		return "", nil
	}
}

// The same scenarios gen-golden.zsh ran through the shell's fire, compared
// line by line with what the shell notified.
func TestFireMatchesShellGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "fire-reports.golden"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	f := newFixture(t)
	f.e.Deliver = func(ctx context.Context, target string) error {
		if strings.Contains(target, "gone") {
			return errors.New("gone")
		}
		return nil
	}
	fire := func(idle, locked string, idleMin map[string]int, targets ...string) string {
		f.r.handle = hsFake(idle, locked)
		f.e.Cfg.Native.IdleMinS = idleMin
		return f.e.Fire(context.Background(), "job", "Reset", targets).Report
	}
	var got []string
	got = append(got, fire("5", "false", nil, "tmux:%1", "kitty:3", "codex:t", "claude-bg:b", "frontmost"))
	got = append(got, fire("fail", "false", nil, "tmux:%1", "codex:t"))
	got = append(got, fire("fail", "false", map[string]int{"tmux": 600}, "tmux:%1", "codex:t"))
	got = append(got, fire("900", "false", map[string]int{"tmux": 600}, "tmux:%1"))
	got = append(got, fire("5", "true", map[string]int{"frontmost": 0}, "frontmost"))
	got = append(got, fire("5", "false", map[string]int{"frontmost": 0}, "frontmost"))
	got = append(got, fire("5", "false", nil, "tmux:%gone", "kitty:3"))
	eqSlice(t, got, want, "fire reports")
	eqSlice(t, f.n.bodies, want, "notifications")
	eq(t, len(f.logLines()), len(want), "one log line per fire")
	eq(t, strings.HasSuffix(f.logLines()[0], " job: "+want[0]), true, "log line format")
}

func TestFireIdleReadOnlyWhenNeeded(t *testing.T) {
	f := newFixture(t)
	f.e.Deliver = func(ctx context.Context, target string) error { return nil }
	f.r.handle = hsFake("fail", "false")
	f.e.Fire(context.Background(), "job", "Reset", []string{"tmux:%1", "kitty:2", "codex:t"})
	eq(t, len(f.r.called("hs")), 0, "Hammerspoon not asked when every kind is 0")

	// Fail safe: an unreadable lock state holds frontmost back.
	f.e.Cfg.Native.IdleMinS = map[string]int{"frontmost": 0}
	f.r.handle = hsFake("5", "fail")
	rep := f.e.Fire(context.Background(), "job", "Reset", []string{"frontmost"})
	eq(t, rep.Report, "Reset -- not resuming frontmost (could not tell whether the screen is locked)", "lock unreadable")

	// A negative setting is not a whole number of seconds.
	f.e.Cfg.Native.IdleMinS = map[string]int{"tmux": -1}
	rep = f.e.Fire(context.Background(), "job", "Reset", []string{"tmux:%1"})
	eq(t, rep.Report, "Reset -- not resuming tmux:%1 (its idle setting is not a whole number of seconds: -1)", "bad setting")

	// Unknown kinds fall back to the general value.
	f.e.Cfg.Native.IdleMinS = map[string]int{"default": 42}
	v, ok := f.e.IdleMin("other:1")
	eq(t, v, 42, "default value")
	eq(t, ok, true, "ok")
	v, _ = f.e.IdleMin("frontmost")
	eq(t, v, 42, "frontmost follows the general value")
	v, _ = f.e.IdleMin("claude-bg:x")
	eq(t, v, 0, "claude_bg defaults to 0")

	rep = f.e.Fire(context.Background(), "job", "Reset", nil)
	eq(t, rep.Report, "Reset -- not resuming: no target was recorded", "no targets")
}

// livePaneRunner answers the commands behind "does pane %1 hold a live
// agent": tmux list-panes and ps.
func livePaneHandler(f *fixture, extra func(argv []string) (string, bool, error)) func(argv []string) (string, error) {
	return func(argv []string) (string, error) {
		if extra != nil {
			if out, ok, err := extra(argv); ok {
				return out, err
			}
		}
		j := strings.Join(argv, " ")
		switch {
		case strings.HasPrefix(j, "tmux list-panes -a"):
			return "%1\t500\t0\n%2\t600\t1\n", nil
		case strings.HasPrefix(j, "ps -axo"):
			return "  700   500\n  500     1\n  800   600\n", nil
		}
		return "", errors.New("unexpected: " + j)
	}
}

func TestSendTmux(t *testing.T) {
	f := newFixture(t)
	tr := f.write(filepath.Join(".claude-work", "projects", "p", idA+".jsonl"), "{}\n")
	f.l.rows = []LiveRow{{PID: "700", ID: idA, Transcript: tr}}
	f.r.handle = livePaneHandler(f.fixture(), func(argv []string) (string, bool, error) {
		j := strings.Join(argv, " ")
		switch {
		case strings.HasPrefix(j, "tmux display-message"):
			return "0\n", true, nil
		case strings.HasPrefix(j, "tmux send-keys"):
			return "", true, nil
		}
		return "", false, nil
	})
	if err := f.e.Send(context.Background(), "tmux:%1"); err != nil {
		t.Fatal(err)
	}
	keys := f.r.called("tmux send-keys")
	eq(t, len(keys), 2, "two send-keys")
	eq(t, keys[0], "tmux send-keys -t %1 -l -- "+DefaultText, "literal text")
	eq(t, keys[1], "tmux send-keys -t %1 Enter", "separate Enter")

	// A pane no longer hosting a live agent is refused before any typing.
	err := f.e.Send(context.Background(), "tmux:%2")
	if err == nil || !strings.Contains(err.Error(), "no longer holds a live agent session") {
		t.Fatalf("dead pane: %v", err)
	}
	eq(t, len(f.r.called("tmux send-keys")), 2, "nothing typed into the dead pane")
}

// fixture() lets helpers take the embedded fixture uniformly.
func (f *fixture) fixture() *fixture { return f }

func TestTmuxSendTextRefusesDeadPane(t *testing.T) {
	f := newFixture(t)
	f.r.handle = func(argv []string) (string, error) {
		if argv[1] == "display-message" {
			return "1\n", nil
		}
		return "", errors.New("unexpected")
	}
	err := f.e.tmuxSendText(context.Background(), "%5", "x")
	if err == nil || !strings.Contains(err.Error(), "has exited") {
		t.Fatalf("dead: %v", err)
	}
	f.r.handle = func(argv []string) (string, error) { return "", nil }
	err = f.e.tmuxSendText(context.Background(), "%5", "x")
	if err == nil || !strings.Contains(err.Error(), "no such tmux pane") {
		t.Fatalf("gone: %v", err)
	}
}

const kittyLSJSON = `[{"tabs":[{"windows":[
 {"id":3,"title":"a","foreground_processes":[{"pid":901,"cmdline":["claude"]}]},
 {"id":4,"title":"b","foreground_processes":[{"pid":902,"cmdline":["tmux","attach"]}]},
 {"id":5,"title":"c","foreground_processes":[{"pid":903,"cmdline":["zsh"]}]}
]}]}]`

func (f *fixture) kittySocket() string {
	sock := f.write(filepath.Join("socks", "kitty-4242.sock"), "")
	return "unix:" + sock
}

func TestSendKitty(t *testing.T) {
	f := newFixture(t)
	sock := f.kittySocket()
	f.r.handle = func(argv []string) (string, error) {
		j := strings.Join(argv, " ")
		switch {
		case j == "kitty @ --to "+sock+" ls":
			return kittyLSJSON, nil
		case strings.HasPrefix(j, "kitty @ --to "+sock+" send-text"):
			return "", nil
		}
		return "", errors.New("unexpected: " + j)
	}
	if err := f.e.Send(context.Background(), "kitty:4"); err != nil {
		t.Fatal(err)
	}
	st := f.r.called("kitty @ --to " + sock + " send-text")
	eqSlice(t, st, []string{"kitty @ --to " + sock + " send-text --match id:4 " + DefaultText + "\r"}, "send-text")

	err := f.e.Send(context.Background(), "kitty:99")
	if err == nil || !strings.Contains(err.Error(), "kitty window 99 is gone") {
		t.Fatalf("gone window: %v", err)
	}
	eq(t, len(f.r.called("kitty @ --to "+sock+" send-text")), 1, "nothing sent to a gone window")
}

func TestSendCodexUsesThreadHome(t *testing.T) {
	f := newFixture(t)
	home := filepath.Join(f.home, "codex-two")
	tr := f.write(filepath.Join(home, "sessions", "2026", "01", "01", "rollout-x-"+idB+".jsonl"), "{}\n")
	if _, err := f.e.Register(Here{Agent: "codex", ID: idB, Transcript: tr, Scope: "codex-alpha"}, []string{"codex:" + idB}); err != nil {
		t.Fatal(err)
	}
	f.r.handle = func(argv []string) (string, error) { return "", nil }
	if err := f.e.Send(context.Background(), "codex:"+idB); err != nil {
		t.Fatal(err)
	}
	eqSlice(t, f.r.joined(), []string{"codex queue --thread " + idB + " --message " + DefaultText}, "queue")
	env := f.r.envs[0]
	found := false
	for _, kv := range env {
		if kv == "CODEX_HOME="+home {
			found = true
		}
		if strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_ID=") {
			t.Fatal("agent markers must not leak into commands")
		}
	}
	eq(t, found, true, "CODEX_HOME of the thread")
}

func TestSendFrontmostQuotesLua(t *testing.T) {
	f := newFixture(t)
	f.e.Cfg.Native.Text = `say "hi" \ then` + "\n"
	f.r.handle = func(argv []string) (string, error) { return "", nil }
	if err := f.e.Send(context.Background(), "frontmost"); err != nil {
		t.Fatal(err)
	}
	calls := f.r.joined()
	eq(t, len(calls), 2, "wake then type")
	eq(t, calls[0], "hs -q -c hs.caffeinate.declareUserActivity() ; return true", "wake")
	eq(t, calls[1], `hs -q -c hs.eventtap.keyStrokes("say \"hi\" \\ then\010"); hs.eventtap.keyStroke({}, "return");`, "typed")
	eq(t, f.slept.Seconds(), 1.0, "a beat after waking")
}

func TestSendClaudeBg(t *testing.T) {
	f := newFixture(t)
	agentsWork := `[{"kind":"background","pid":11,"id":"abc123","state":"idle","sessionId":"` + idA + `"},
	               {"kind":"background","pid":null,"id":"dead01","sessionId":"` + idB + `"}]`
	captures := 0
	f.r.handle = func(argv []string) (string, error) {
		j := strings.Join(argv, " ")
		switch {
		case j == "claude agents --json":
			return "[]", nil
		case strings.HasPrefix(j, "tmux new-session"):
			return "%42\n", nil
		case strings.HasPrefix(j, "tmux capture-pane"):
			captures++
			if captures < 3 {
				return "loading\n", nil
			}
			return "❯ \n", nil
		case strings.HasPrefix(j, "tmux display-message"):
			return "0\n", nil
		case strings.HasPrefix(j, "tmux"):
			return "", nil
		}
		return "", errors.New("unexpected: " + j)
	}
	// The work seat answers with the background session.
	base := f.r.handle
	f.r.handle = func(argv []string) (string, error) {
		f.r.mu.Lock()
		env := f.r.envs[len(f.r.envs)-1]
		f.r.mu.Unlock()
		if strings.Join(argv, " ") == "claude agents --json" {
			for _, kv := range env {
				if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
					return agentsWork, nil
				}
			}
		}
		return base(argv)
	}
	if err := f.e.Send(context.Background(), "claude-bg:"+idA); err != nil {
		t.Fatal(err)
	}
	ns := f.r.called("tmux new-session")
	eq(t, len(ns), 1, "one scratch session")
	eq(t, ns[0], "tmux new-session -d -s agent-usage-attach-abc123 -x 120 -y 30 -P -F #{pane_id} env -u CLAUDE_CONFIG_DIR CLAUDE_CONFIG_DIR="+filepath.Join(f.home, ".claude-work")+" claude attach abc123", "attach under the seat's home")
	eqSlice(t, f.r.called("tmux send-keys"), []string{
		"tmux send-keys -t %42 -l -- " + DefaultText,
		"tmux send-keys -t %42 Enter",
		"tmux send-keys -t %42 C-z",
	}, "typed, then detached")
	kills := f.r.called("tmux kill-session")
	eq(t, len(kills), 2, "replace any old scratch, then kill it")

	err := f.e.Send(context.Background(), "claude-bg:"+idB)
	if err == nil || !strings.Contains(err.Error(), "no running background session") {
		t.Fatalf("finished session: %v", err)
	}
}

func TestPaseoTargetIsNotOurs(t *testing.T) {
	f := newFixture(t)
	err := f.e.Send(context.Background(), "paseo:"+idA)
	eq(t, errors.Is(err, ErrPaseoTarget), true, "paseo")
	err = f.e.Send(context.Background(), "paseo:nope")
	if err == nil || errors.Is(err, ErrPaseoTarget) {
		t.Fatalf("bad paseo id: %v", err)
	}
	if err := f.e.Send(context.Background(), "carrier-pigeon:1"); err == nil {
		t.Fatal("unknown target must fail")
	}
}

func TestCommandNotifier(t *testing.T) {
	r := &fakeRunner{handle: func(argv []string) (string, error) { return "", nil }}
	n := CommandNotifier{Runner: r, Command: []string{"notify-cmd", "--group", "x"}}
	n.Notify(context.Background(), "T", "B")
	n = CommandNotifier{Runner: r}
	n.Notify(context.Background(), "Auto-continue", `a "b" \c`)
	eqSlice(t, r.joined(), []string{
		"notify-cmd --group x T B",
		`osascript -e display notification "a \"b\" \\c" with title "Auto-continue"`,
	}, "notifier argv")
}
