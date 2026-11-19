package keeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// Every id, path and name below is synthetic.

const (
	agentA   = "00000000-0000-4000-8000-00000000000a"
	agentB   = "00000000-0000-4000-8000-00000000000b"
	agentC   = "00000000-0000-4000-8000-00000000000c"
	agentD   = "00000000-0000-4000-8000-00000000000d"
	agentE   = "00000000-0000-4000-8000-00000000000e"
	sessionA = "11111111-1111-4111-8111-11111111111a"
	sessionB = "11111111-1111-4111-8111-11111111111b"
	sessionC = "11111111-1111-4111-8111-11111111111c"
	sessionD = "11111111-1111-4111-8111-11111111111d"
	sessionE = "11111111-1111-4111-8111-11111111111e"
	t0       = 1_800_000_000.0 // a fixed epoch for the fake clock
)

type sentMsg struct{ agent, text, behavior string }

type fakePaseo struct {
	mu       sync.Mutex
	inspects map[string]*paseo.Object
	logs     map[string][]*paseo.Object
	list     []*paseo.Object
	sent     []sentMsg
	sendErr  error
	inspErr  error
	onSend   func(agent, text string)
	// calls counts Inspect, List, Logs and Send; delay slows Inspect and
	// List down.
	calls map[string]int
	delay time.Duration
}

func (f *fakePaseo) count(name string) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
	d := f.delay
	f.mu.Unlock()
	if d > 0 && (name == "inspect" || name == "list") {
		time.Sleep(d)
	}
}

func (f *fakePaseo) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakePaseo) Inspect(_ context.Context, id, _ string) (*paseo.Object, error) {
	f.count("inspect")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspErr != nil {
		return nil, f.inspErr
	}
	o, ok := f.inspects[id]
	if !ok {
		return nil, errors.New("no such agent")
	}
	return o.Clone(), nil
}

func (f *fakePaseo) Send(_ context.Context, id, _, text, behavior string) error {
	f.count("send")
	f.mu.Lock()
	if f.sendErr != nil {
		f.mu.Unlock()
		return f.sendErr
	}
	f.sent = append(f.sent, sentMsg{id, text, behavior})
	hook := f.onSend
	f.mu.Unlock()
	if hook != nil {
		hook(id, text)
	}
	return nil
}

func (f *fakePaseo) Logs(_ context.Context, id, _ string, _ time.Time) ([]*paseo.Object, error) {
	f.count("logs")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[id], nil
}

func (f *fakePaseo) List(context.Context, string) ([]*paseo.Object, error) {
	f.count("list")
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*paseo.Object(nil), f.list...), nil
}

func (f *fakePaseo) sends() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

type fakeSeats struct{ seat Seat }

func (s *fakeSeats) Seat(context.Context, paseo.Identity) (Seat, error) { return s.seat, nil }

type harness struct {
	t      *testing.T
	root   string
	user   string
	home   string
	now    float64
	cfg    *config.Config
	eng    *paseo.Engine
	paseo  *fakePaseo
	seats  *fakeSeats
	k      *Keeper
	nextID int
}

func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, root: realTemp(t), now: t0}
	h.user = filepath.Join(h.root, "user")
	h.home = filepath.Join(h.root, "paseo")
	for _, d := range []string{filepath.Join(h.user, ".claude", "projects", "p"), filepath.Join(h.home, "agents", "p")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	compaction := filepath.Join(h.root, "compaction.yaml")
	if err := os.WriteFile(compaction, []byte("# synthetic\nmin_context_tokens: 100000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(h.root, "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	on := true
	cfg.Keeper.Enabled = &on
	cfg.Keeper.CompactionFile = compaction
	cfg.Paseo.Homes = []string{h.home}
	cfg.Paseo.RegistrationsDir = filepath.Join(h.root, "registrations")
	h.cfg = cfg
	h.eng = paseo.New(cfg, nil)
	h.eng.UserHome = h.user
	h.eng.Getenv = func(string) string { return "" }
	h.paseo = &fakePaseo{inspects: map[string]*paseo.Object{}, logs: map[string][]*paseo.Object{}}
	h.seats = &fakeSeats{seat: Seat{Account: "claude:default", Verdict: model.Usable}}
	h.k = New(cfg, h.eng, h.paseo, h.seats, log.New(io.Discard, "", 0))
	h.k.Now = func() time.Time { return model.Time(h.now) }
	h.k.NewID = func() string { h.nextID++; return fmt.Sprintf("c-%06d", h.nextID) }
	return h
}

func iso(at float64) string { return model.Time(at).UTC().Format(time.RFC3339Nano) }

// agent creates a Claude agent: Paseo's record, its inspect answer and an
// empty transcript.
func (h *harness) agent(id, session string) {
	h.record(id, session, "idle", nil)
	h.paseo.inspects[id] = paseo.Obj("Id", id, "Name", "synthetic agent", "Provider", "claude",
		"Cwd", "/tmp/example-project", "Status", "idle", "Archived", false, "PendingPermissions", []any{})
	h.transcript(session, "")
}

// record writes Paseo's stored record; extra overrides keys.
func (h *harness) record(id, session, status string, extra map[string]any) {
	r := paseo.Obj("id", id, "provider", "claude", "cwd", "/tmp/example-project", "lastStatus", status,
		"lastActivityAt", iso(h.now), "persistence", paseo.Obj("sessionId", session))
	for k, v := range extra {
		r.Set(k, v)
	}
	text, err := paseo.DumpIndent(r, 2)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.home, "agents", "p", id+".json"), []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) setInspect(id, key string, v any) { h.paseo.inspects[id].Set(key, v) }

func (h *harness) transcriptPath(session string) string {
	return filepath.Join(h.user, ".claude", "projects", "p", session+".jsonl")
}

func (h *harness) transcript(session, text string) {
	if err := os.WriteFile(h.transcriptPath(session), []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) appendT(session string, lines ...string) {
	f, err := os.OpenFile(h.transcriptPath(session), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
}

func jsonLine(v map[string]any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func userLine(at float64, text string) string {
	return jsonLine(map[string]any{"type": "user", "timestamp": iso(at),
		"message": map[string]any{"role": "user", "content": text}})
}

func toolResultLine(at float64) string {
	return jsonLine(map[string]any{"type": "user", "timestamp": iso(at),
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "ok"}}}})
}

func assistantLine(at float64, tokens int64) string {
	return jsonLine(map[string]any{"type": "assistant", "timestamp": iso(at),
		"message": map[string]any{"role": "assistant", "model": "claude-example",
			"content": []any{map[string]any{"type": "text", "text": "ok"}},
			"usage":   map[string]any{"input_tokens": 2, "cache_creation_input_tokens": 100, "cache_read_input_tokens": tokens - 102}}})
}

func limitLine(at float64) string {
	return jsonLine(map[string]any{"type": "assistant", "timestamp": iso(at), "isApiErrorMessage": true,
		"error": "rate_limit", "uuid": fmt.Sprintf("u-%d", int64(at)),
		"message": map[string]any{"role": "assistant", "model": "<synthetic>",
			"content": []any{map[string]any{"type": "text", "text": "You've hit your limit"}}}})
}

func compactBoundaryLine(at float64) string {
	return jsonLine(map[string]any{"type": "system", "subtype": "compact_boundary", "timestamp": iso(at)})
}

// realTurn appends a user prompt and a reply with the given context.
func (h *harness) realTurn(session string, at float64, tokens int64) {
	h.appendT(session, userLine(at-30, "synthetic task"), toolResultLine(at-20), assistantLine(at, tokens))
}

func (h *harness) path(id string) string {
	p, err := h.eng.RegistrationPath(id, h.home)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) cache(id string) *Cache {
	state, err := paseo.ReadObject(h.path(id))
	if err != nil {
		h.t.Fatal(err)
	}
	c, err := readCache(state)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) set(id, policy string) View {
	v, err := h.k.SetPolicy(context.Background(), id, h.home, policy, 0, "cli")
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

// view observes the agent (as the loop would, with paseo inspect) and
// reads its document from memory.
func (h *harness) view(id string) View {
	if o, err := h.k.observe(context.Background(), id, h.home, true); err == nil {
		h.k.store(o)
	}
	v, err := h.k.Agent(context.Background(), id, h.home)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func (h *harness) step(id string) { h.k.Step(context.Background(), h.path(id)) }

func (h *harness) wantSends(n int) []sentMsg {
	h.t.Helper()
	s := h.paseo.sends()
	if len(s) != n {
		h.t.Fatalf("sends = %d %+v, want %d", len(s), s, n)
	}
	return s
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
