package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claudeEnv(f *fixture, id, pane string) map[string]string {
	env := map[string]string{
		"HOME": f.home, "CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": id,
		"CLAUDE_CONFIG_DIR": filepath.Join(f.home, ".claude-work"),
	}
	if pane != "" {
		env["TMUX_PANE"] = pane
	}
	return env
}

func TestOnTmuxThenOffStatusList(t *testing.T) {
	f := newFixture(t)
	tr := f.write(filepath.Join(".claude-work", "projects", "p", idA+".jsonl"), "{}\n")
	f.l.rows = []LiveRow{{PID: "700", ID: idA, Transcript: tr}}
	f.r.handle = livePaneHandler(f, func(argv []string) (string, bool, error) {
		if strings.Join(argv, " ") == "claude agents --json" {
			return "[]", true, nil
		}
		return "", false, nil
	})
	env := claudeEnv(f, idA, "%1")
	res, err := f.e.On(context.Background(), env, OnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eqSlice(t, res.Registration.Targets, []string{"tmux:%1"}, "tmux target")
	eq(t, res.Registration.Transcript, tr, "transcript resolved from the id")
	eq(t, res.Message, "auto-continue on: claude-work, resumes tmux:%1 when the limit resets; the agent-quotas daemon checks it whenever claude:work changes", "message")

	st, err := f.e.Status(env, OnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, st.Format(), "this session: on (claude-work, targets: tmux:%1)\n", "status")

	f.write(filepath.Join(f.e.scopeDir("claude-work"), ".blocked_at"), "1800000000\n")
	f.write(filepath.Join(f.e.scopeDir("claude-work"), ".blocked-claude-"+idA), "")
	ls, err := f.e.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := "claude-work\n  blocked since " + Fmt3339(1800000000) + "\n  live  claude " + idA + "  -> tmux:%1  (its own turn hit the limit)\n"
	eq(t, FormatList(ls), want, "list")

	off, err := f.e.Off(env, OnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, off.Message, "auto-continue off: claude-work has no registered session left", "off")
	eq(t, f.exists(f.e.scopeDir("claude-work")), false, "scope directory removed with its stamps")
	off, _ = f.e.Off(env, OnOptions{})
	eq(t, off.Message, "auto-continue was not on for this session", "off again")
	st, _ = f.e.Status(env, OnOptions{})
	eq(t, st.Format(), "this session: off (claude-work)\n", "status off")
	ls, _ = f.e.List(context.Background(), false)
	eq(t, FormatList(ls), "agent-auto-continue-list: nothing registered\n", "empty list")
}

func TestOffLeavesOthers(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{idA, idB} {
		f.e.Register(Here{Agent: "claude", ID: id, Scope: "claude-work"}, []string{"tmux:%1"})
	}
	res, _ := f.e.Unregister("claude-work", "claude", idA, "claude "+idA)
	eq(t, res.Message, "auto-continue off for claude "+idA+"; claude-work still has 1 registered", "message")
}

func TestListPrune(t *testing.T) {
	f := newFixture(t)
	f.e.Register(Here{Agent: "claude", ID: idA, Scope: "claude-work"}, []string{"tmux:%1"})
	f.e.Register(Here{Agent: "codex", ID: idB, Scope: "codex-alpha"}, []string{"codex:" + idB})
	f.l.rows = []LiveRow{{ID: idB}}
	ls, _ := f.e.List(context.Background(), false)
	eq(t, FormatList(ls), "claude-work\n  dead  claude "+idA+"  -> tmux:%1\ncodex-alpha\n  live  codex "+idB+"  -> codex:"+idB+"\n", "dead and live")
	ls, _ = f.e.List(context.Background(), true)
	eqSlice(t, ls[0].Pruned, []string{"claude-" + idA}, "pruned")
	eq(t, f.exists(f.e.regFile("claude-work", "claude", idA)), false, "removed")

	f.l.err = errors.New("broken")
	f.e.Register(Here{Agent: "claude", ID: idA, Scope: "claude-work"}, []string{"tmux:%1"})
	ls, _ = f.e.List(context.Background(), true)
	eq(t, len(ls[0].Pruned), 0, "a failed listing prunes nothing")
}

func TestResolveTargets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	got, _ := f.e.ResolveTargets(ctx, "claude", idA, "", nil, TargetOptions{Frontmost: true})
	eqSlice(t, got, []string{"frontmost"}, "frontmost only when asked")
	got, _ = f.e.ResolveTargets(ctx, "codex", idA, "", nil, TargetOptions{})
	eqSlice(t, got, []string{"codex:" + idA}, "codex is queued")

	// A background Claude session wins over the pane.
	f.r.handle = func(argv []string) (string, error) {
		if strings.Join(argv, " ") == "claude agents --json" {
			return `[{"kind":"background","pid":3,"id":"s1","sessionId":"` + idA + `"}]`, nil
		}
		return "", errors.New("unexpected")
	}
	got, _ = f.e.ResolveTargets(ctx, "claude", idA, "", map[string]string{"TMUX_PANE": "%1"}, TargetOptions{})
	eqSlice(t, got, []string{"claude-bg:" + idA}, "claude-bg")

	// Nothing reaches it: the error lists what was tried.
	f.r.handle = func(argv []string) (string, error) {
		j := strings.Join(argv, " ")
		if j == "claude agents --json" {
			return "[]", nil
		}
		if strings.HasPrefix(j, "tmux list-panes") || strings.HasPrefix(j, "ps ") {
			return "", nil
		}
		return "", errors.New("unexpected: " + j)
	}
	_, err := f.e.ResolveTargets(ctx, "claude", idA, "", map[string]string{"TMUX_PANE": "%1"}, TargetOptions{})
	eq(t, err.Error(), "no way to reach this session: tmux pane %1 is not listed as holding a live agent session; no transcript yet, so no kitty window can be matched. Run with --frontmost to type wherever the keyboard focus is at the time", "error")
	_, err = f.e.ResolveTargets(ctx, "agy", idA, "/x/t.jsonl", map[string]string{}, TargetOptions{})
	eq(t, err.Error(), "no way to reach this session: not inside tmux; no kitty window shows this session. Run with --frontmost to type wherever the keyboard focus is at the time", "error outside tmux")
}

func TestResolveTargetsOutsideFindsPaneByTranscript(t *testing.T) {
	f := newFixture(t)
	tr := f.write(filepath.Join(".claude-work", "projects", "p", idA+".jsonl"), "{}\n")
	f.l.rows = []LiveRow{{PID: "700", ID: idA, Transcript: tr}}
	f.r.handle = livePaneHandler(f, func(argv []string) (string, bool, error) {
		if strings.Join(argv, " ") == "claude agents --json" {
			return "[]", true, nil
		}
		return "", false, nil
	})
	got, err := f.e.ResolveTargets(context.Background(), "claude", idA, tr, map[string]string{"TMUX_PANE": "%9"}, TargetOptions{Outside: true})
	eq(t, err, error(nil), "err")
	eqSlice(t, got, []string{"tmux:%1"}, "pane by transcript, not the caller's pane")
}

func TestResolveTargetsKitty(t *testing.T) {
	f := newFixture(t)
	sock := f.kittySocket()
	ta := f.write(filepath.Join(".claude-work", "projects", "p", idA+".jsonl"), "{}\n")
	tb := f.write(filepath.Join(".claude-work", "projects", "p", idB+".jsonl"), "{}\n")
	tc := f.write(filepath.Join(".claude-work", "projects", "p", idC+".jsonl"), "{}\n")
	// A runs in window 3 itself; B sits in a tmux session window 4's client
	// is attached to; C is known only from the window registry (window 5).
	f.l.rows = []LiveRow{{PID: "901", ID: idA, Transcript: ta}, {PID: "999", ID: idB, Transcript: tb, Tmux: "+Claude b"}}
	f.write(filepath.Join(".local", "state", "agent-sessions", "4242-5"), tc+"\n")
	f.r.handle = func(argv []string) (string, error) {
		j := strings.Join(argv, " ")
		switch {
		case j == "claude agents --json":
			return "[]", nil
		case j == "kitty @ --to "+sock+" ls":
			return kittyLSJSON, nil
		case strings.HasPrefix(j, "tmux list-clients"):
			return "902\t+Claude b\n", nil
		case strings.HasPrefix(j, "tmux list-sessions"):
			return "+Claude b\t\n", nil
		case strings.HasPrefix(j, "tmux list-panes"), strings.HasPrefix(j, "ps "):
			return "", nil
		}
		return "", errors.New("unexpected: " + j)
	}
	ctx := context.Background()
	for _, c := range []struct{ id, tr, want string }{{idA, ta, "kitty:3"}, {idB, tb, "kitty:4"}, {idC, tc, "kitty:5"}} {
		got, err := f.e.ResolveTargets(ctx, "claude", c.id, c.tr, map[string]string{}, TargetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		eqSlice(t, got, []string{c.want}, "kitty window of "+c.id)
	}
}

func TestHookVariants(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.e.Register(Here{Agent: "agy", ID: idA, Scope: "agy"}, []string{"tmux:%1"})
	f.e.Register(Here{Agent: "codex", ID: idB, Scope: "codex-alpha"}, []string{"codex:" + idB})

	res, _ := f.e.Hook(ctx, "claude", "", []byte(`{"session_id":"`+idC+`"}`))
	eq(t, res, HookResult{}, "unregistered: no-op")
	res, _ = f.e.Hook(ctx, "claude", "", []byte(`not json`))
	eq(t, res, HookResult{}, "garbage: no-op")

	res, _ = f.e.Hook(ctx, "agy", "%1", []byte(`{"conversationId":"`+idA+`","terminationReason":"completed"}`))
	eq(t, res, HookResult{Scope: "agy"}, "agy turn end without error kicks but does not mark")
	eq(t, f.e.marked("agy", "agy", idA), false, "no mark")

	res, _ = f.e.Hook(ctx, "agy", "%1", []byte(`{"conversationId":"`+idA+`","terminationReason":"error"}`))
	eq(t, res, HookResult{Marked: true, Debounced: true}, "second kick within a minute is debounced, but marks")
	f.now = f.now.Add(61 * time.Second)
	res, _ = f.e.Hook(ctx, "agy", "%1", []byte(`{"conversationId":"`+idA+`"}`))
	eq(t, res.Scope, "agy", "kick after the debounce")

	res, _ = f.e.Hook(ctx, "codex", "", []byte(`{"session_id":"`+idB+`","agent_id":"sub-1"}`))
	eq(t, res, HookResult{}, "a subagent's payload is ignored")
	res, _ = f.e.Hook(ctx, "codex", "", []byte(`{"session_id":"`+idB+`","agent_id":null}`))
	eq(t, res, HookResult{Scope: "codex-alpha"}, "codex root thread kicks without marking")
}

func TestCommandListerAddsSpecs(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(filepath.Join(f.home, ".claude", "projects"), 0o755)
	os.MkdirAll(filepath.Join(f.home, ".claude-work", "projects"), 0o755)
	os.MkdirAll(filepath.Join(f.home, ".codex", "sessions"), 0o755)
	f.r.handle = func(argv []string) (string, error) {
		return "123\t" + idA + "\tname\t/cwd\t/t.jsonl\t-\tidle\tinteractive\n\n", nil
	}
	l := &CommandLister{Engine: f.e}
	rows, err := l.Live(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	eqSlice(t, f.r.joined(), []string{"agent_session live-all claude=" + filepath.Join(f.home, ".claude", "projects") +
		" claude=" + filepath.Join(f.home, ".claude-work", "projects") +
		" codex=" + filepath.Join(f.home, ".codex", "sessions")}, "specs from the config, existing roots only")
	eq(t, rows[0], LiveRow{PID: "123", ID: idA, Name: "name", Cwd: "/cwd", Transcript: "/t.jsonl", Tmux: "-", Status: "idle", Kind: "interactive"}, "row")

	f.e.Cfg.Native.LiveListCommand = []string{"agent_session", "live-all", "claude=/x"}
	l.Live(context.Background())
	eq(t, f.r.joined()[1], "agent_session live-all claude=/x", "explicit specs are kept as they are")
}
