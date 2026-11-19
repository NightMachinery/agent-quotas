package paseo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
)

// Port of test_paseo_auto_continue.py. Every id below is synthetic.

type sent struct{ agentID, home, text, behavior string }

type fakePaseo struct {
	snapshot  *Object
	snapshots []*Object // consumed first, as the Python iterator
	sent      []sent
	sendHook  func() error
}

func (f *fakePaseo) Inspect(_ context.Context, agentID, home string) (*Object, error) {
	if len(f.snapshots) > 0 {
		s := f.snapshots[0]
		f.snapshots = f.snapshots[1:]
		return s, nil
	}
	return f.snapshot, nil
}

func (f *fakePaseo) Send(_ context.Context, agentID, home, text, behavior string) error {
	if f.sendHook != nil {
		if err := f.sendHook(); err != nil {
			return err
		}
	}
	f.sent = append(f.sent, sent{agentID, home, text, behavior})
	return nil
}

type quotaCall struct{ kind, a, b string }

type fakeQuotas struct {
	usable bool
	err    error
	claude func(profile, family string) (bool, error)
	agy    func(account, model string) (bool, error)
	calls  []quotaCall
}

func (f *fakeQuotas) Claude(_ context.Context, profile, family string) (bool, error) {
	f.calls = append(f.calls, quotaCall{"claude", profile, family})
	if f.claude != nil {
		return f.claude(profile, family)
	}
	return f.usable, f.err
}

func (f *fakeQuotas) Codex(_ context.Context, codexHome string) (bool, error) {
	f.calls = append(f.calls, quotaCall{"codex", codexHome, ""})
	return f.usable, f.err
}

func (f *fakeQuotas) Agy(_ context.Context, account, model string) (bool, error) {
	f.calls = append(f.calls, quotaCall{"agy", account, model})
	if f.agy != nil {
		return f.agy(account, model)
	}
	return f.usable, f.err
}

type harness struct {
	t        *testing.T
	root     string
	user     string
	home     string
	path     string
	config   *Object
	provider string
	snapshot *Object
	record   *Object
	records  []*Object
	state    *Object
	paseo    *fakePaseo
	quotas   *fakeQuotas
	e        *Engine
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, root: realTemp(t), provider: "codex-work"}
	h.user = filepath.Join(h.root, "user")
	h.home = filepath.Join(h.root, "daemon")
	for _, d := range []string{h.home, filepath.Join(h.user, ".claude"), filepath.Join(h.user, ".claude-work")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.config = Obj("agents", Obj("providers", Obj(h.provider,
		Obj("extends", "codex", "label", "Work", "env", Obj("CODEX_HOME", "/tmp/example-codex-work")))))
	h.writeConfig()
	h.snapshot = Obj("Id", "agent-id", "Provider", h.provider, "Cwd", "/tmp/project",
		"Status", "error", "Archived", false, "PendingPermissions", []any{})
	h.record = Obj("id", "agent-id", "lastStatus", "error", "lastError", "Usage limit reached",
		"lastUserMessageAt", "turn-1", "attentionTimestamp", "error-1")
	h.paseo = &fakePaseo{snapshot: h.snapshot}
	h.quotas = &fakeQuotas{usable: true}
	cfg := &config.Config{
		Claude: config.Claude{Profiles: []config.ClaudeProfile{{Name: "default"}}},
		Paseo:  config.Paseo{RegistrationsDir: filepath.Join(h.root, "private")},
	}
	h.e = &Engine{Config: cfg, Paseo: h.paseo, Quotas: h.quotas, UserHome: h.user,
		Getenv: func(string) string { return "" }}
	h.e.storedFn = func(agentID, home string) (*Object, error) {
		if len(h.records) > 0 {
			r := h.records[0]
			h.records = h.records[1:]
			return r, nil
		}
		return h.record, nil
	}
	h.state = Obj("enabled", true, "agent_id", "agent-id", "home", h.home, "binding", h.binding(h.snapshot), "poll", 300)
	h.path = filepath.Join(h.root, "state.json")
	h.write()
	return h
}

func (h *harness) writeConfig() {
	text, err := Dumps(h.config, false)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.home, "config.json"), []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) binding(snapshot *Object) *Object {
	b, err := h.e.binding(snapshot, h.home)
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

func (h *harness) write() {
	if err := atomicWrite(h.path, h.state); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) read() *Object {
	o, err := readObject(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	return o
}

func (h *harness) tick() string {
	h.t.Helper()
	status, err := h.e.Tick(context.Background(), h.path)
	if err != nil {
		h.t.Fatalf("tick: %v", err)
	}
	return status
}

func TestQuotaRecoverySendsToExactAgentAndHome(t *testing.T) {
	h := newHarness(t)
	if s := h.tick(); !strings.Contains(s, "sent") {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 1 {
		t.Fatalf("sent %d", len(h.paseo.sent))
	}
	got := h.paseo.sent[0]
	if got != (sent{"agent-id", h.home, "Continue.", "steer"}) {
		t.Fatalf("%+v", got)
	}
	// The Codex seat asked about is the bound CODEX_HOME, resolved.
	if want := (quotaCall{"codex", realpath("/tmp/example-codex-work"), ""}); !reflect.DeepEqual(h.quotas.calls, []quotaCall{want}) {
		t.Fatalf("quota calls %+v", h.quotas.calls)
	}
}

func TestSameFailureSendsOnceAcrossRestart(t *testing.T) {
	h := newHarness(t)
	h.tick()
	h.tick()
	if len(h.paseo.sent) != 1 {
		t.Fatal(len(h.paseo.sent))
	}
}

func TestNewFailedTurnCanContinue(t *testing.T) {
	h := newHarness(t)
	h.tick()
	h.record.Set("lastUserMessageAt", "turn-2")
	h.record.Set("attentionTimestamp", "error-2")
	h.tick()
	if len(h.paseo.sent) != 2 {
		t.Fatal(len(h.paseo.sent))
	}
}

func TestExhaustedAccountWaits(t *testing.T) {
	h := newHarness(t)
	h.quotas.usable = false
	if s := h.tick(); s != StatusWaitingQuota {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
	h.quotas.usable = true
	h.tick()
	if len(h.paseo.sent) != 1 {
		t.Fatal(len(h.paseo.sent))
	}
}

func TestCollectedErrorContinuesOnceAfterRecovery(t *testing.T) {
	h := newHarness(t)
	h.quotas.usable = false
	if s := h.tick(); s != StatusWaitingQuota {
		t.Fatal(s)
	}
	h.snapshot.Set("Status", "closed")
	h.record.Set("lastStatus", "closed")
	if s := h.tick(); s != StatusWaitingQuota {
		t.Fatal(s)
	}
	h.quotas.usable = true
	if s := h.tick(); !strings.Contains(s, "sent") {
		t.Fatal(s)
	}
	h.tick()
	if len(h.paseo.sent) != 1 {
		t.Fatal(len(h.paseo.sent))
	}
}

func TestCollectionDoesNotRepeatClaimedError(t *testing.T) {
	h := newHarness(t)
	h.tick()
	h.snapshot.Set("Status", "closed")
	h.record.Set("lastStatus", "closed")
	h.tick()
	if len(h.paseo.sent) != 1 {
		t.Fatal(len(h.paseo.sent))
	}
}

func TestCollectedAgentWithoutQuotaFailureStaysStopped(t *testing.T) {
	for _, lastError := range []any{nil, "Network connection failed"} {
		h := newHarness(t)
		h.e.transcriptFn = func(*Object, Identity) (string, error) { return "", nil }
		h.snapshot.Set("Status", "closed")
		h.record.Set("lastStatus", "closed")
		h.record.Set("lastError", lastError)
		h.tick()
		if len(h.paseo.sent) != 0 {
			t.Fatalf("%v: sent", lastError)
		}
	}
}

func TestIdleAgentNeverContinues(t *testing.T) {
	h := newHarness(t)
	h.e.transcriptFn = func(*Object, Identity) (string, error) { return "", nil }
	h.snapshot.Set("Status", "idle")
	h.record.Set("lastStatus", "idle")
	h.tick()
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestNonquotaFailureNeverContinues(t *testing.T) {
	h := newHarness(t)
	h.record.Set("lastError", "Network connection failed")
	h.tick()
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestRunningAgentNeverContinues(t *testing.T) {
	h := newHarness(t)
	h.snapshot.Set("Status", "running")
	h.tick()
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestPendingPermissionNeverContinues(t *testing.T) {
	h := newHarness(t)
	h.snapshot.Set("PendingPermissions", []any{"permission"})
	if s := h.tick(); s != StatusWaitingFailure {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestArchivedAgentDisables(t *testing.T) {
	h := newHarness(t)
	h.snapshot.Set("Archived", true)
	if s := h.tick(); s != StatusUnavailable {
		t.Fatal(s)
	}
	if truthy(h.read().Value("enabled")) || len(h.paseo.sent) != 0 {
		t.Fatal("still enabled or sent")
	}
}

func TestReplacedAgentDisables(t *testing.T) {
	h := newHarness(t)
	h.snapshot.Set("Id", "another-agent")
	h.tick()
	if truthy(h.read().Value("enabled")) {
		t.Fatal("still enabled")
	}
}

func TestProfileChangeDisables(t *testing.T) {
	h := newHarness(t)
	providers := h.config.Value("agents").(*Object).Value("providers").(*Object)
	providers.Value(h.provider).(*Object).Value("env").(*Object).Set("CODEX_HOME", "/tmp/other-home")
	h.writeConfig()
	if s := h.tick(); s != StatusProviderChange {
		t.Fatal(s)
	}
	if truthy(h.read().Value("enabled")) || len(h.paseo.sent) != 0 {
		t.Fatal("still enabled or sent")
	}
}

func TestLabelsDoNotChangeProfileBinding(t *testing.T) {
	h := newHarness(t)
	providers := h.config.Value("agents").(*Object).Value("providers").(*Object)
	providers.Value(h.provider).(*Object).Set("label", "Better label")
	h.writeConfig()
	h.tick()
	if len(h.paseo.sent) != 1 {
		t.Fatal(len(h.paseo.sent))
	}
}

func TestManualContinuationDuringQuotaQuerySkips(t *testing.T) {
	h := newHarness(t)
	fresh := h.snapshot.Clone()
	fresh.Set("Status", "running")
	h.paseo.snapshots = []*Object{h.snapshot, fresh}
	if s := h.tick(); s != StatusChanged {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestNewTurnDuringQuotaQuerySkips(t *testing.T) {
	h := newHarness(t)
	fresh := h.record.Clone()
	fresh.Set("lastUserMessageAt", "new-user-turn")
	h.records = []*Object{h.record, fresh}
	if s := h.tick(); s != StatusChanged {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestSendFailureClaimedAndDisabled(t *testing.T) {
	h := newHarness(t)
	h.paseo.sendHook = func() error {
		if s, _ := h.read().String("status"); s != StatusSending {
			t.Errorf("status during send: %s", s)
		}
		return errors.New("timeout")
	}
	status, err := h.e.Tick(context.Background(), h.path)
	if err == nil || status != StatusUncertain {
		t.Fatalf("%q %v", status, err)
	}
	state := h.read()
	if truthy(state.Value("enabled")) || !truthy(state.Value("claimed_failure")) {
		t.Fatal("not disabled or not claimed")
	}
	h.paseo.sendHook = nil
	if s := h.tick(); s != StatusOff {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestAccountQueryFailureDoesNotSend(t *testing.T) {
	h := newHarness(t)
	h.quotas.err = errors.New("unavailable")
	if _, err := h.e.Tick(context.Background(), h.path); err == nil {
		t.Fatal("no error")
	}
	if len(h.paseo.sent) != 0 || !truthy(h.read().Value("enabled")) {
		t.Fatal("sent or disabled")
	}
}

func TestOffDoesNotInspectOrSend(t *testing.T) {
	h := newHarness(t)
	h.state.Set("enabled", false)
	h.write()
	h.paseo.snapshot = nil // an inspect would now fail the tick
	if s := h.tick(); s != StatusOff {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestClaudeProfileResolutionAndInheritance(t *testing.T) {
	h := newHarness(t)
	work := filepath.Join(h.user, ".claude-work")
	h.config = Obj("agents", Obj("providers", Obj("named", Obj("extends", "intermediate"),
		"intermediate", Obj("extends", "claude", "env", Obj("CLAUDE_CONFIG_DIR", work)))))
	h.writeConfig()
	id, err := h.e.providerConfig(h.home, "named")
	if err != nil || id.Profile != "work" || id.NativeHome != work {
		t.Fatalf("%+v %v", id, err)
	}
	id, err = h.e.providerConfig(h.home, "claude")
	if err != nil || id.Profile != "default" {
		t.Fatalf("%+v %v", id, err)
	}
	// An unknown Claude directory is refused with the old message.
	h.config = Obj("agents", Obj("providers", Obj("odd", Obj("extends", "claude", "env", Obj("CLAUDE_CONFIG_DIR", "/tmp/example-unknown-claude")))))
	h.writeConfig()
	if _, err := h.e.providerConfig(h.home, "odd"); err == nil || !strings.Contains(err.Error(), "default and work profiles") {
		t.Fatal(err)
	}
}

func TestClaudeProfileFromConfiguredProfiles(t *testing.T) {
	h := newHarness(t)
	h.e.Config.Claude.Profiles = []config.ClaudeProfile{
		{Name: "default"}, {Name: "explicit"}, {Name: "seat-b", ConfigDir: "~/.claude-b"}}
	h.e.Config.Claude.Profiles[1].ConfigDir = "~/.claude"
	if err := os.MkdirAll(filepath.Join(h.user, ".claude-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.config = Obj("agents", Obj("providers", Obj(
		"b", Obj("extends", "claude", "env", Obj("CLAUDE_CONFIG_DIR", "~/.claude-b")),
		"explicit-default", Obj("extends", "claude", "env", Obj("CLAUDE_CONFIG_DIR", "~/.claude")),
		"work", Obj("extends", "claude", "env", Obj("CLAUDE_CONFIG_DIR", "~/.claude-work")))))
	h.writeConfig()
	for provider, want := range map[string]string{"b": "seat-b", "explicit-default": "explicit", "claude": "default", "work": "work"} {
		id, err := h.e.providerConfig(h.home, provider)
		if err != nil || id.Profile != want {
			t.Errorf("%s: %+v %v, want %s", provider, id, err, want)
		}
	}
}

func TestProviderCycleRefused(t *testing.T) {
	h := newHarness(t)
	providers := h.config.Value("agents").(*Object).Value("providers").(*Object)
	providers.Value(h.provider).(*Object).Set("extends", h.provider)
	h.writeConfig()
	var refusal *Error
	if _, err := h.e.providerConfig(h.home, h.provider); !errors.As(err, &refusal) {
		t.Fatal(err)
	}
}

func writeRecords(t *testing.T, dir string, records ...*Object) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		id, _ := r.String("id")
		text, _ := Dumps(r, false)
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoppedAgentsScan(t *testing.T) {
	h := newHarness(t)
	p := h.provider
	writeRecords(t, filepath.Join(h.home, "agents", "-tmp-project"),
		Obj("id", "codex-quota", "provider", p, "lastStatus", "closed", "lastError", "You have hit your usage limit."),
		Obj("id", "codex-other-error", "provider", p, "lastStatus", "error", "lastError", "Bad Request"),
		Obj("id", "codex-archived", "provider", p, "lastStatus", "error", "lastError", "Usage limit reached", "archivedAt", "2026-11-01T00:00:00Z"),
		Obj("id", "codex-running", "provider", p, "lastStatus", "running", "lastError", "Usage limit reached"),
		Obj("id", "claude-quota", "provider", "claude", "lastStatus", "idle"),
		Obj("id", "claude-finished", "provider", "claude", "lastStatus", "idle"),
	)
	if err := os.WriteFile(filepath.Join(h.home, "agents", "broken.json"), []byte("[1]"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.e.transcriptFn = func(r *Object, _ Identity) (string, error) {
		if id, _ := r.String("id"); id == "claude-quota" {
			return "x", nil
		}
		return "", nil
	}
	found, err := h.e.Stopped(context.Background(), h.home, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	if !reflect.DeepEqual(found, []string{"claude-quota", "codex-quota"}) {
		t.Fatal(found)
	}
}

func TestUsableAgentsFilter(t *testing.T) {
	h := newHarness(t)
	h.config = Obj("agents", Obj("providers", Obj("claude-work",
		Obj("extends", "claude", "env", Obj("CLAUDE_CONFIG_DIR", "~/.claude-work")))))
	h.writeConfig()
	records := map[string]*Object{
		"work-opus":   Obj("provider", "claude-work", "config", Obj("model", "claude-opus-5-5")),
		"work-opus-2": Obj("provider", "claude-work", "config", Obj("model", "claude-opus-5-5")),
		"work-sonnet": Obj("provider", "claude-work", "config", Obj("model", "claude-sonnet-5-5")),
		"codex":       Obj("provider", "codex", "runtimeInfo", Obj("model", "gpt-6.1")),
		"broken":      Obj("provider", "odd", "config", NewObject()),
	}
	h.e.storedFn = func(agentID, _ string) (*Object, error) {
		if r, ok := records[agentID]; ok {
			return r, nil
		}
		return nil, refuse("missing")
	}
	h.e.checkableFn = func(provider any) bool { return provider != "odd" }
	h.quotas.claude = func(profile, family string) (bool, error) { return family != "opus", nil }
	var warnings []string
	found, err := h.e.usableAgents(context.Background(), h.home,
		[]string{"work-opus", "work-opus-2", "work-sonnet", "codex", "broken", "gone"},
		func(s string) { warnings = append(warnings, s) })
	if err != nil {
		t.Fatal(err)
	}
	// An agent whose quota cannot be read here is kept, with a note.
	if !reflect.DeepEqual(found, []string{"work-sonnet", "codex", "broken"}) {
		t.Fatal(found)
	}
	// One check per seat and model family, not per agent.
	want := []quotaCall{{"claude", "work", "opus"}, {"claude", "work", "sonnet"}, {"codex", filepath.Join(h.user, ".codex"), ""}}
	if !reflect.DeepEqual(h.quotas.calls, want) {
		t.Fatalf("%+v", h.quotas.calls)
	}
	wantWarn := []string{"broken: odd quota is not checked here; kept", "gone: no readable record; left out"}
	if !reflect.DeepEqual(warnings, wantWarn) {
		t.Fatal(warnings)
	}
}

func TestUsableAgentsFailedSeatCheckLeftOut(t *testing.T) {
	h := newHarness(t)
	h.e.storedFn = func(string, string) (*Object, error) {
		return Obj("provider", "codex", "config", Obj("model", "gpt-6.1")), nil
	}
	h.quotas.err = errors.New("boom")
	var warnings []string
	found, _ := h.e.usableAgents(context.Background(), h.home, []string{"a", "b"}, func(s string) { warnings = append(warnings, s) })
	if len(found) != 0 || !reflect.DeepEqual(warnings, []string{"codex: quota check failed (boom); its agents are left out"}) {
		t.Fatal(found, warnings)
	}
}

func TestAgyAccountResolution(t *testing.T) {
	h := newHarness(t)
	h.config = Obj("agents", Obj("providers", Obj(
		"agy-acp", Obj("extends", "acp", "command", []any{"/x/bin/agy", "acp"}, "env", Obj("AGY_ACCOUNT_ID", "cave")),
		"agy-main", Obj("extends", "acp", "command", []any{"/x/bin/agy"}, "env", Obj("AGY_ACCOUNT_ID", "acc1")),
		"other-acp", Obj("extends", "acp", "command", []any{"/x/bin/gemini"}))))
	h.writeConfig()
	for provider, want := range map[string]string{"agy-acp": "cave", "agy-main": "m", "antigravity": "m", "other-acp": "", "copilot": ""} {
		account, ok, err := agyAccount(h.home, provider)
		if err != nil || account != want || ok != (want != "") {
			t.Errorf("%s: %q %v %v", provider, account, ok, err)
		}
	}
}

func TestAgyQuotaByModel(t *testing.T) {
	h := newHarness(t)
	records := map[string]*Object{
		"hydrogen": Obj("provider", "antigravity", "config", Obj("model", "gemini-4-hydrogen-high")),
		"argon":    Obj("provider", "antigravity", "config", Obj("model", "gemini-4-argon-medium")),
		"stale":    Obj("provider", "agy-cave", "config", Obj("model", "gemini-4-hydrogen-high")),
		"twin":     Obj("provider", "antigravity", "config", Obj("model", "gemini-4-hydrogen-high")),
	}
	h.e.storedFn = func(agentID, _ string) (*Object, error) { return records[agentID], nil }
	h.e.checkableFn = func(any) bool { return false }
	h.e.agyAccountFn = func(provider any) (string, bool, error) {
		if provider == "agy-cave" {
			return "cave", true, nil
		}
		return "m", true, nil
	}
	h.quotas.agy = func(account, model string) (bool, error) {
		if account == "cave" {
			return false, errors.New("stale cache")
		}
		return !strings.Contains(model, "argon"), nil
	}
	var warnings []string
	found, err := h.e.usableAgents(context.Background(), h.home, []string{"hydrogen", "argon", "stale", "twin"},
		func(s string) { warnings = append(warnings, s) })
	if err != nil {
		t.Fatal(err)
	}
	// The exhausted Argon pool hides its agent; an unknown quota keeps one, with a note.
	if !reflect.DeepEqual(found, []string{"hydrogen", "stale", "twin"}) {
		t.Fatal(found)
	}
	if len(h.quotas.calls) != 3 {
		t.Fatalf("%+v", h.quotas.calls)
	}
	if !reflect.DeepEqual(warnings, []string{"Antigravity cave gemini-4-hydrogen-high: quota unknown (stale cache); its agents are kept"}) {
		t.Fatal(warnings)
	}
}

func TestModelFamily(t *testing.T) {
	for in, want := range map[string]string{"claude-opus-5-5": "opus", "claude-Sonnet-4": "sonnet", "gpt-6.1-sol": "gpt", "": ""} {
		if got := ModelFamily(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestClaudeExplicitDefaultConfigDirAsksDefaultProfile(t *testing.T) {
	h := newHarness(t)
	h.config = Obj("agents", Obj("providers", Obj("named", Obj("extends", "claude",
		"env", Obj("CLAUDE_CONFIG_DIR", filepath.Join(h.user, ".claude"))))))
	h.writeConfig()
	h.snapshot.Set("Provider", "named")
	h.snapshot.Set("Model", "claude-opus-4-8")
	h.state.Set("binding", h.binding(h.snapshot))
	h.write()
	h.tick()
	if !reflect.DeepEqual(h.quotas.calls, []quotaCall{{"claude", "default", "opus"}}) {
		t.Fatalf("%+v", h.quotas.calls)
	}
}

func (h *harness) onFixture(agentID string) string {
	h.snapshot.Set("Id", agentID)
	path, err := h.e.RegistrationPath(agentID, h.home)
	if err != nil {
		h.t.Fatal(err)
	}
	return path
}

func TestRepeatedOnPreservesConsumedFailure(t *testing.T) {
	h := newHarness(t)
	agentID := "00000000-0000-4000-8000-000000000001"
	target := h.onFixture(agentID)
	h.state.Set("claimed_failure", "already-sent")
	h.state.Set("status", "already continued")
	if err := atomicWrite(target, h.state); err != nil {
		t.Fatal(err)
	}
	out, err := h.e.On(context.Background(), agentID, h.home, 300)
	if err != nil {
		t.Fatal(err)
	}
	if want := "auto-continue on: codex/codex, target: paseo:" + agentID + ", poll: 300s"; out != want {
		t.Fatal(out)
	}
	state, _ := readObject(target)
	if c, _ := state.String("claimed_failure"); c != "already-sent" {
		t.Fatal(c)
	}
	if s, _ := state.String("status"); s != "already continued" {
		t.Fatal(s)
	}
}

func TestOnWritesPythonCompatibleRegistration(t *testing.T) {
	h := newHarness(t)
	agentID := "00000000-0000-4000-8000-000000000002"
	path := h.onFixture(agentID)
	h.e.Now = func() time.Time { return time.Unix(1789000000, 500000000) }
	if _, err := h.e.On(context.Background(), "{"+strings.ToUpper(agentID)+"}", h.home, 120); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := DecodeObject(data)
	wantKeys := []string{"version", "enabled", "home", "agent_id", "binding", "poll", "status", "registered_at", "set_by"}
	if !reflect.DeepEqual(state.Keys(), wantKeys) {
		t.Fatal(state.Keys())
	}
	if !strings.Contains(string(data), `"registered_at": 1789000000.5`) || !strings.HasSuffix(string(data), "}\n") {
		t.Fatal(string(data))
	}
	if b, _ := state.Object("binding"); !reflect.DeepEqual(b.Keys(), []string{"agent", "profile", "native_home", "config_hash", "provider", "cwd"}) {
		t.Fatal(b.Keys())
	}
	if filepath.Base(path) != FileName(h.home, agentID) {
		t.Fatal(path)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	status, err := h.e.Status(context.Background(), agentID, h.home)
	if err != nil {
		t.Fatal(err)
	}
	want := "this Paseo agent: on, waiting for a quota-failed turn\nprofile: codex/codex\ntarget: paseo:" + agentID +
		", poll: 120s\nwatcher: agent-quotas daemon"
	if status != want {
		t.Fatal(status)
	}
	regs, err := h.e.Registrations()
	if err != nil || len(regs) != 1 || regs[0].Path != path || regs[0].Poll != 120 || !regs[0].Enabled ||
		regs[0].Agent != "codex" || regs[0].NativeHome != realpath("/tmp/example-codex-work") {
		t.Fatalf("%+v %v", regs, err)
	}
	off, err := h.e.Off(context.Background(), agentID, h.home)
	if err != nil || off != "auto-continue off for paseo:"+agentID {
		t.Fatal(off, err)
	}
	if s, _ := h.e.Status(context.Background(), agentID, h.home); !strings.HasPrefix(s, "this Paseo agent: off, off\n") {
		t.Fatal(s)
	}
}

func TestOnValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.e.On(ctx, "not-a-uuid", h.home, 300); err == nil || err.Error() != "an exact agent UUID is required" {
		t.Fatal(err)
	}
	if _, err := h.e.On(ctx, "00000000-0000-4000-8000-000000000003", h.home, 29); err == nil || err.Error() != "--poll must be at least 30 seconds" {
		t.Fatal(err)
	}
	if _, err := h.e.On(ctx, "", h.home, 300); err == nil {
		t.Fatal("empty id accepted")
	}
	h.snapshot.Set("Id", "00000000-0000-4000-8000-000000000003")
	h.snapshot.Set("Archived", true)
	if _, err := h.e.On(ctx, "00000000-0000-4000-8000-000000000003", h.home, 300); err == nil || err.Error() != "Agent is unavailable or archived" {
		t.Fatal(err)
	}
	h.e.Getenv = func(name string) string {
		if name == "PASEO_HOST" {
			return "example.test:6767"
		}
		return ""
	}
	if _, err := h.e.On(ctx, "00000000-0000-4000-8000-000000000003", h.home, 300); err == nil || !strings.Contains(err.Error(), "remote hosts") {
		t.Fatal(err)
	}
	h.e.Getenv = func(string) string { return "" }
	if s, _ := h.e.Off(ctx, "00000000-0000-4000-8000-000000000009", h.home); s != "auto-continue was not on for this Paseo agent" {
		t.Fatal(s)
	}
	if s, _ := h.e.Status(ctx, "00000000-0000-4000-8000-000000000009", h.home); s != "this Paseo agent: off" {
		t.Fatal(s)
	}
}

func TestRegistrationsListsEnabledOnly(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.root, "regs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.e.Config.Paseo.RegistrationsDir = dir
	if err := atomicWrite(filepath.Join(dir, "a.json"), h.state); err != nil {
		t.Fatal(err)
	}
	other := h.state.Clone()
	other.Set("agent_id", "other")
	other.Set("enabled", false)
	if err := atomicWrite(filepath.Join(dir, "b.json"), other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := h.e.List()
	if err != nil || !reflect.DeepEqual(rows, []string{"agent-id\t" + h.home + "\tcodex/codex\t"}) {
		t.Fatal(rows, err)
	}
	regs, _ := h.e.Registrations()
	if len(regs) != 2 || regs[1].Enabled {
		t.Fatalf("%+v", regs)
	}
	h.e.Config.Paseo.RegistrationsDir = filepath.Join(h.root, "missing")
	if rows, err := h.e.List(); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}

func TestRegistrationModeIsPrivate(t *testing.T) {
	h := newHarness(t)
	info, _ := os.Stat(h.path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
}

func TestRegistrationSymlinkRefused(t *testing.T) {
	h := newHarness(t)
	link := filepath.Join(h.root, "link")
	if err := os.Symlink(h.path, link); err != nil {
		t.Fatal(err)
	}
	var refusal *Error
	if err := atomicWrite(link, NewObject()); !errors.As(err, &refusal) {
		t.Fatal(err)
	}
}

func TestPrivateDirSymlinkRefused(t *testing.T) {
	root := realTemp(t)
	target := filepath.Join(root, "private")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var refusal *Error
	if _, err := privateDir(link); !errors.As(err, &refusal) {
		t.Fatal(err)
	}
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := privateDir(open); !errors.As(err, &refusal) {
		t.Fatal(err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(realTemp(t), "x.lock")
	unlock, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		u, err := lockFile(path)
		if err == nil {
			u()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second lock taken while held")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	<-got
}

// Codex limit stops: the last root task_complete in the session log carries the error.

type codexFixture struct {
	native string
	log    string
	record *Object
}

const codexThread = "01a1bbc1-518f-7bb0-b332-515f9a0527ee"

func newCodexFixture(t *testing.T) *codexFixture {
	native := filepath.Join(realTemp(t), "codex-home")
	day := time.UnixMilli(0x01a1bbc1518f).Local().Format("2006/01/02")
	log := filepath.Join(native, "sessions", day, "rollout-2026-11-08T16-22-51-"+codexThread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		t.Fatal(err)
	}
	return &codexFixture{native: native, log: log, record: Obj("id", "agent-id", "lastStatus", "idle",
		"persistence", Obj("sessionId", codexThread, "metadata", Obj("threadId", codexThread)))}
}

func event(kind, turn, root, errMsg string) *Object {
	if root == "" {
		root = turn
	}
	payload := Obj("type", kind, "turn_id", turn, "root_turn_id", root)
	if errMsg != "" {
		payload.Set("error", Obj("message", errMsg))
	}
	return Obj("type", "event_msg", "payload", payload)
}

func writeJSONL(t *testing.T, path string, entries ...*Object) {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		s, _ := Dumps(e, false)
		b.WriteString(s + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *codexFixture) failure(t *testing.T) string {
	got, err := codexLimitFailure(f.record, f.native, tailBytes)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCodexLimitOnLastRootTurnIsAFailure(t *testing.T) {
	f := newCodexFixture(t)
	limit := "You’ve hit your usage limit. Visit https://example.test/settings/usage"
	writeJSONL(t, f.log, event("task_started", "t1", "", ""), event("task_complete", "t1", "", limit),
		event("task_complete", "r1", "t0", ""))
	if f.failure(t) == "" {
		t.Fatal("no failure")
	}
}

func TestCodexLaterTurnOrOtherErrorIsNot(t *testing.T) {
	f := newCodexFixture(t)
	limit := "You’ve hit your usage limit."
	writeJSONL(t, f.log, event("task_complete", "t1", "", limit), event("task_started", "t2", "", ""))
	if f.failure(t) != "" {
		t.Fatal("later turn")
	}
	writeJSONL(t, f.log, event("task_complete", "t1", "", "Bad Request"))
	if f.failure(t) != "" {
		t.Fatal("other error")
	}
	writeJSONL(t, f.log, event("task_complete", "t1", "", ""))
	if f.failure(t) != "" {
		t.Fatal("no error")
	}
}

func TestCodexMissingLogIsNotAFailure(t *testing.T) {
	f := newCodexFixture(t)
	if f.failure(t) != "" {
		t.Fatal("failure without log")
	}
}

func TestCodexRolloutFullSearch(t *testing.T) {
	f := newCodexFixture(t)
	elsewhere := filepath.Join(f.native, "sessions", "2001", "01", "01", filepath.Base(f.log))
	if err := os.MkdirAll(filepath.Dir(elsewhere), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, elsewhere, event("task_complete", "t1", "", "usage limit"))
	if got := codexRollout(f.native, codexThread); got != elsewhere {
		t.Fatal(got)
	}
}

// Claude limit stops: Paseo shows the agent idle, the transcript shows the limit.

const claudeSession = "00000000-0000-4000-8000-0000000000aa"

type claudeFixture struct {
	*harness
	native     string
	transcript string
}

func newClaudeFixture(t *testing.T) *claudeFixture {
	h := newHarness(t)
	h.config = NewObject()
	h.writeConfig()
	f := &claudeFixture{harness: h, native: filepath.Join(h.root, "claude-home")}
	f.transcript = filepath.Join(f.native, "projects", "-tmp-project", claudeSession+".jsonl")
	if err := os.MkdirAll(filepath.Dir(f.transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	h.snapshot.Set("Provider", "claude")
	h.snapshot.Set("Status", "idle")
	h.record = Obj("id", "agent-id", "lastStatus", "idle", "lastError", nil,
		"persistence", Obj("provider", "claude", "sessionId", claudeSession))
	h.state.Set("binding", h.binding(h.snapshot))
	h.write()
	h.e.transcriptFn = func(r *Object, _ Identity) (string, error) {
		return claudeLimitFailure(r, f.native, tailBytes)
	}
	return f
}

func limitEntry(uid string) *Object {
	return Obj("type", "assistant", "uuid", uid, "isApiErrorMessage", true, "error", "rate_limit",
		"message", Obj("model", "<synthetic>", "content", []any{
			Obj("type", "text", "text", "You've hit your session limit · resets 3am")}))
}

func userEntry(text string) *Object { return Obj("type", "user", "message", Obj("content", text)) }

func (f *claudeFixture) write(entries ...*Object) { writeJSONL(f.t, f.transcript, entries...) }

func (f *claudeFixture) failure() string {
	got, err := claudeLimitFailure(f.record, f.native, tailBytes)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func TestClaudeLimitAsLastEntryIsAFailure(t *testing.T) {
	f := newClaudeFixture(t)
	f.write(userEntry("go"), limitEntry("limit-1"), Obj("type", "queue-operation"), Obj("type", "last-prompt"))
	if f.failure() == "" {
		t.Fatal("no failure")
	}
}

func TestClaudeLaterUserTurnClearsIt(t *testing.T) {
	f := newClaudeFixture(t)
	f.write(limitEntry("limit-1"), userEntry("Continue"))
	if f.failure() != "" {
		t.Fatal("still a failure")
	}
}

func TestClaudeOrdinaryFinishedTurnIsNotAFailure(t *testing.T) {
	f := newClaudeFixture(t)
	f.write(Obj("type", "assistant", "message", Obj("content", []any{Obj("type", "text", "text", "Done.")})))
	if f.failure() != "" {
		t.Fatal("failure")
	}
}

func TestClaudeOtherAPIErrorsAreNotQuota(t *testing.T) {
	f := newClaudeFixture(t)
	other := limitEntry("limit-1")
	other.Set("error", "server_error")
	other.Set("message", Obj("content", []any{Obj("type", "text", "text", "API Error: 500")}))
	f.write(other)
	if f.failure() != "" {
		t.Fatal("failure")
	}
}

func TestClaudeTruncatedTailLineIsSkipped(t *testing.T) {
	f := newClaudeFixture(t)
	f.write(userEntry(strings.Repeat("x", 5000)), limitEntry("limit-1"))
	got, err := claudeLimitFailure(f.record, f.native, 600)
	if err != nil || got == "" {
		t.Fatal(got, err)
	}
}

func TestClaudeMissingOrMalformedSessionIsNone(t *testing.T) {
	f := newClaudeFixture(t)
	f.record.Set("persistence", Obj("sessionId", "../escape"))
	if f.failure() != "" {
		t.Fatal("escape")
	}
	f.record.Set("persistence", Obj("sessionId", "00000000-0000-4000-8000-0000000000bb"))
	if f.failure() != "" {
		t.Fatal("missing")
	}
}

func TestIdleClaudeAgentAfterLimitContinuesOnce(t *testing.T) {
	f := newClaudeFixture(t)
	f.write(limitEntry("limit-1"))
	if s := f.tick(); !strings.Contains(s, "sent") {
		t.Fatal(s)
	}
	f.tick()
	if len(f.paseo.sent) != 1 || f.paseo.sent[0].agentID != "agent-id" {
		t.Fatalf("%+v", f.paseo.sent)
	}
	if !reflect.DeepEqual(f.quotas.calls, []quotaCall{{"claude", "default", ""}}) {
		t.Fatalf("%+v", f.quotas.calls)
	}
}

func TestCollectedClaudeAgentAfterLimitContinuesOnce(t *testing.T) {
	f := newClaudeFixture(t)
	f.write(limitEntry("limit-1"))
	f.snapshot.Set("Status", "closed")
	f.record.Set("lastStatus", "closed")
	if s := f.tick(); !strings.Contains(s, "sent") {
		t.Fatal(s)
	}
	f.tick()
	if len(f.paseo.sent) != 1 {
		t.Fatal(len(f.paseo.sent))
	}
}

func TestCollectedClaudeAgentKeepsEligibilityChecks(t *testing.T) {
	f := newClaudeFixture(t)
	f.snapshot.Set("Status", "closed")
	f.record.Set("lastStatus", "closed")
	f.write(limitEntry("limit-1"))
	f.snapshot.Set("PendingPermissions", []any{"permission"})
	f.tick()
	if len(f.paseo.sent) != 0 {
		t.Fatal("sent with a pending permission")
	}
	f.snapshot.Set("PendingPermissions", []any{})
	f.record.Set("archivedAt", "archived")
	f.tick()
	if len(f.paseo.sent) != 0 {
		t.Fatal("sent while archived")
	}
	f.record.Delete("archivedAt")
	f.write(limitEntry("limit-1"), userEntry("Continue"))
	f.tick()
	if len(f.paseo.sent) != 0 {
		t.Fatal("sent after a later turn")
	}
}

func TestCollectedClaudeTurnChangeDuringQuotaCheckSkips(t *testing.T) {
	f := newClaudeFixture(t)
	f.snapshot.Set("Status", "closed")
	f.record.Set("lastStatus", "closed")
	f.write(limitEntry("limit-1"))
	f.quotas.claude = func(string, string) (bool, error) {
		f.write(limitEntry("limit-1"), userEntry("Continue"))
		return true, nil
	}
	if s := f.tick(); s != StatusChanged {
		t.Fatal(s)
	}
	if len(f.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestSecondLimitStopContinuesAgain(t *testing.T) {
	f := newClaudeFixture(t)
	f.write(limitEntry("limit-1"))
	f.tick()
	f.write(limitEntry("limit-1"), userEntry("Continue"), limitEntry("limit-2"))
	f.tick()
	if len(f.paseo.sent) != 2 {
		t.Fatal(len(f.paseo.sent))
	}
}

func assertCodexAgentResumesOnItsLoggedLimit(t *testing.T, f *claudeFixture) {
	f.config = Obj("agents", Obj("providers", Obj("codex-x", Obj("extends", "codex"))))
	f.writeConfig()
	f.snapshot.Set("Provider", "codex-x")
	f.state.Set("binding", f.binding(f.snapshot))
	f.harness.write()
	seen := map[string]bool{}
	f.e.transcriptFn = func(_ *Object, id Identity) (string, error) {
		seen[id.Agent] = true
		return "codex-failure", nil
	}
	f.tick()
	if !reflect.DeepEqual(seen, map[string]bool{"codex": true}) || len(f.paseo.sent) == 0 {
		t.Fatal(seen, f.paseo.sent)
	}
}

func TestIdleCodexAgentResumesOnItsLoggedLimit(t *testing.T) {
	assertCodexAgentResumesOnItsLoggedLimit(t, newClaudeFixture(t))
}

func TestCollectedCodexAgentResumesOnItsLoggedLimit(t *testing.T) {
	f := newClaudeFixture(t)
	f.snapshot.Set("Status", "closed")
	f.record.Set("lastStatus", "closed")
	assertCodexAgentResumesOnItsLoggedLimit(t, f)
}

func TestRunningClaudeAgentNeverContinues(t *testing.T) {
	f := newClaudeFixture(t)
	f.write(limitEntry("limit-1"))
	f.snapshot.Set("Status", "running")
	f.tick()
	if len(f.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

// A registration the Python tool wrote, with its claimed failure, stays
// claimed: the Go digest of the same turn is the same string.
func TestPythonClaimStaysClaimed(t *testing.T) {
	h := newHarness(t)
	var golden map[string]string
	readGolden(t, "failures-golden.json", &golden)
	h.record, _ = DecodeObject([]byte(`{"id": "agent-id", "lastStatus": "error", "lastError": "You’ve hit your usage limit",
		"lastUserMessageAt": "2026-11-01T00:00:00.000Z", "attentionTimestamp": 1789000000123}`))
	h.state.Set("claimed_failure", golden["last_error"])
	h.write()
	if s := h.tick(); s != StatusWaitingNew {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestSendBehaviorOrder(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if SendBehavior("", get) != "steer" {
		t.Fatal("default")
	}
	env["PASEO_SEND_BEHAVIOR"] = "interrupt"
	if SendBehavior("", get) != "interrupt" {
		t.Fatal("PASEO_SEND_BEHAVIOR")
	}
	env["PASEO_HANDOFF_SEND_BEHAVIOR"] = "queue"
	if SendBehavior("", get) != "queue" || SendBehavior("steer", get) != "steer" {
		t.Fatal("order")
	}
	h := newHarness(t)
	h.e.Config.Paseo.SendBehavior = "interrupt"
	h.tick()
	if h.paseo.sent[0].behavior != "interrupt" {
		t.Fatal(h.paseo.sent)
	}
}

func TestFileNameAndListFields(t *testing.T) {
	if got := FileName("/tmp/example-home/.paseo", "x"); got != mustDigest("/tmp/example-home/.paseo")[:16]+"-x.json" {
		t.Fatal(got)
	}
	if got := pySplitJoin(" a\t b\x1c\nc "); got != "a b c" {
		t.Fatalf("pySplitJoin = %q", got)
	}
}
