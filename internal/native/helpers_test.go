package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// fakeRunner answers commands from a handler and records every call. Nothing
// real is ever run.
type fakeRunner struct {
	mu     sync.Mutex
	calls  [][]string
	envs   [][]string
	handle func(argv []string) (string, error)
}

func (f *fakeRunner) Run(ctx context.Context, c procx.Cmd) (procx.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), c.Argv...))
	f.envs = append(f.envs, append([]string(nil), c.Env...))
	h := f.handle
	f.mu.Unlock()
	if h == nil {
		return procx.Result{}, errors.New("fake: no handler")
	}
	out, err := h(c.Argv)
	return procx.Result{Stdout: []byte(out)}, err
}

// joined returns every call as one space-joined string.
func (f *fakeRunner) joined() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func (f *fakeRunner) called(prefix string) []string {
	var out []string
	for _, c := range f.joined() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

type fakeNotifier struct {
	mu     sync.Mutex
	bodies []string
}

func (n *fakeNotifier) Notify(ctx context.Context, title, body string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.bodies = append(n.bodies, body)
	return nil
}

type fakeLister struct {
	rows []LiveRow
	err  error
}

func (l *fakeLister) Live(ctx context.Context) ([]LiveRow, error) { return l.rows, l.err }

type fixture struct {
	t      *testing.T
	home   string
	e      *Engine
	r      *fakeRunner
	n      *fakeNotifier
	l      *fakeLister
	now    time.Time
	slept  time.Duration
	sentMu sync.Mutex
}

var testNow = time.Unix(1_800_000_000, 0)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("KITTY_LISTEN_ON", "")
	t.Setenv("NIGHT_SOCKETS_DIR", filepath.Join(home, "socks"))
	cfg, err := config.Load(filepath.Join(home, "no-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Claude.Profiles = []config.ClaudeProfile{{Name: "default"}, {Name: "work", ConfigDir: "~/.claude-work"}}
	cfg.Native.LogFile = filepath.Join(home, "logs", "fire.log")
	f := &fixture{t: t, home: home, now: testNow}
	r := &fakeRunner{handle: func(argv []string) (string, error) { return "", errors.New("unexpected: " + strings.Join(argv, " ")) }}
	e := New(cfg, r)
	n := &fakeNotifier{}
	l := &fakeLister{}
	e.Notifier = n
	e.Lister = l
	e.Now = func() time.Time { return f.now }
	e.Sleep = func(ctx context.Context, d time.Duration) error { f.slept += d; return nil }
	e.ProcAlive = func(pid int) bool { return true }
	f.e, f.r, f.n, f.l = e, r, n, l
	return f
}

func (f *fixture) write(path, content string) string {
	f.t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(f.home, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fixture) setMtime(path string, t time.Time) {
	f.t.Helper()
	if err := os.Chtimes(path, t, t); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (f *fixture) logLines() []string {
	data, _ := os.ReadFile(f.e.logFile())
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// transcript writes a Claude transcript whose last assistant record names
// modelID, with the given mtime.
func (f *fixture) claudeTranscript(name, modelID string, mtime time.Time) string {
	content := `{"type":"user","message":{"content":"hi"}}` + "\n"
	if modelID != "" {
		content += `{"type":"assistant","message":{"model":"` + modelID + `","content":[]}}` + "\n"
	}
	p := f.write(filepath.Join(".claude-work", "projects", "p", name+".jsonl"), content)
	f.setMtime(p, mtime)
	return p
}

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s:\n got: %#v\nwant: %#v", what, got, want)
	}
}

func eqSlice(t *testing.T, got, want []string, what string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") || len(got) != len(want) {
		t.Fatalf("%s:\n got: %q\nwant: %q", what, got, want)
	}
}
