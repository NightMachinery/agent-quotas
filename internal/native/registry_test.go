package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	idA = "00000000-0000-4000-8000-00000000000a"
	idB = "00000000-0000-4000-8000-00000000000b"
	idC = "00000000-0000-4000-8000-00000000000c"
)

// The shell's own register function wrote testdata/registration.golden
// (gen-golden.zsh); reading it and writing it back must give the same bytes.
func TestRegistrationGoldenRoundTrip(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "registration.golden"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRegistration("claude-work", string(data))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, r.Agent, "claude", "agent")
	eq(t, r.ID, "00000000-0000-4000-8000-000000000001", "id")
	eq(t, strings.Join(r.Targets, " "), "tmux:%7 kitty:12", "targets keep the shell's order")
	eq(t, r.Line()+"\n", string(data), "round trip")

	f := newFixture(t)
	res, err := f.e.Register(Here{Agent: r.Agent, ID: r.ID, Transcript: r.Transcript, Scope: "claude-work"}, r.Targets)
	if err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(f.e.regFile("claude-work", r.Agent, r.ID))
	want := strings.Replace(string(data), "\t"+strings.TrimSpace(strings.Split(string(data), "\t")[4]), "\t1800000000", 1)
	eq(t, string(written), want, "written file")
	eq(t, res.Registration.RegisteredAt, int64(1800000000), "registered at")
}

// A registry as the shell leaves it: registrations plus the dotfiles, which
// are not registrations.
func TestReadShellRegistry(t *testing.T) {
	f := newFixture(t)
	dir := f.e.scopeDir("claude-work")
	f.write(filepath.Join(dir, "claude-"+idA), "claude\t"+idA+"\t/x/a.jsonl\ttmux:%3\t1700000000\n")
	f.write(filepath.Join(dir, "claude-"+idB), "claude\t"+idB+"\t\tkitty:4 frontmost\t1700000001\n")
	f.write(filepath.Join(dir, ".blocked_at"), "1700000100\n")
	f.write(filepath.Join(dir, ".blocked-claude-"+idA), "")
	f.write(filepath.Join(dir, ".kicked"), "")
	f.write(filepath.Join(dir, ".fn_ran"), "")
	f.write(filepath.Join(f.e.scopeDir("codex-alpha"), ".kicked"), "") // only a stamp: not a scope

	scopes, err := f.e.Scopes()
	if err != nil {
		t.Fatal(err)
	}
	eqSlice(t, scopes, []string{"claude-work"}, "scopes")
	regs, err := f.e.Registrations("claude-work")
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 2 {
		t.Fatalf("want 2 registrations, got %d", len(regs))
	}
	eq(t, regs[0].Transcript, "/x/a.jsonl", "transcript")
	eq(t, regs[1].Transcript, "", "empty transcript kept as a field")
	eqSlice(t, regs[1].Targets, []string{"kitty:4", "frontmost"}, "targets")
	v, ok := f.e.blockedSince("claude-work")
	eq(t, ok, true, "blocked_at read")
	eq(t, v, int64(1700000100), "blocked_at value")
	eq(t, f.e.marked("claude-work", "claude", idA), true, "mark")
	eq(t, f.e.marked("claude-work", "claude", idB), false, "no mark")

	r, ok := f.e.FindRegistration("claude", idB)
	eq(t, ok, true, "find")
	eq(t, r.Scope, "claude-work", "found scope")
	_, ok = f.e.FindRegistration("claude", "../x")
	eq(t, ok, false, "path traversal refused")
}

func TestAgentScopeFromEnv(t *testing.T) {
	f := newFixture(t)
	home := f.home

	a, err := AgentOf(map[string]string{"CLAUDECODE": "1", "CODEX_THREAD_ID": "x"})
	eq(t, a, "claude", "claude wins")
	eq(t, err, error(nil), "err")
	a, _ = AgentOf(map[string]string{"CODEX_SANDBOX": "seatbelt"})
	eq(t, a, "codex", "codex")
	a, _ = AgentOf(map[string]string{"AI_AGENT": "agy_1"})
	eq(t, a, "agy", "agy")
	_, err = AgentOf(map[string]string{})
	eq(t, err, ErrNotAgent, "no agent")

	eq(t, f.e.ClaudeProfileOf(map[string]string{}), "default", "unset is default")
	eq(t, f.e.ClaudeProfileOf(map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude-work") + "/"}), "work", "work")
	eq(t, f.e.ClaudeProfileOf(map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude")}), "default", "explicit default dir")
	eq(t, f.e.ClaudeProfileOf(map[string]string{"CLAUDE_CONFIG_DIR": "/elsewhere/.claude-third"}), "claude-third", "unregistered dir")

	// Codex: the snapshot whose bytes match auth.json names the scope.
	ch := filepath.Join(home, "codexhome")
	f.write(filepath.Join(ch, "auth.json"), `{"a":2}`)
	f.write(filepath.Join(ch, "auth_alpha.json"), `{"a":1}`)
	f.write(filepath.Join(ch, "auth-beta.json"), `{"a":2}`)
	s, _ := f.e.ScopeOf("codex", map[string]string{"CODEX_HOME": ch})
	eq(t, s, "codex-beta", "byte match")
	os.WriteFile(filepath.Join(ch, "auth.json"), []byte(`{"a":3}`), 0o644)
	s, _ = f.e.ScopeOf("codex", map[string]string{"CODEX_HOME": ch})
	eq(t, s, "codex-auth", "no match is auth")
	s, _ = f.e.ScopeOf("codex", map[string]string{"CODEX_HOME": filepath.Join(home, "none")})
	eq(t, s, "codex-auth", "no auth.json is auth")
	s, _ = f.e.ScopeOf("agy", nil)
	eq(t, s, "agy", "agy")

	eq(t, f.e.AccountForScope("claude-work"), "claude:work", "claude account")
	eq(t, f.e.AccountForScope("codex-beta"), "codex:beta", "codex account")
	eq(t, f.e.AccountForScope("agy"), "agy:m", "agy account")

	_, err = f.e.HereOf(map[string]string{"PASEO_AGENT_ID": "x", "CLAUDECODE": "1"}, OnOptions{})
	eq(t, err, ErrPaseoAgent, "paseo is not ours")
}

func TestResolveTranscript(t *testing.T) {
	f := newFixture(t)
	p := f.write(filepath.Join(".claude-work", "projects", "p", idA+".jsonl"), "{}\n")
	eq(t, ResolveTranscript("claude", idA, map[string]string{"HOME": f.home, "CLAUDE_CONFIG_DIR": filepath.Join(f.home, ".claude-work")}), p, "claude")
	c := f.write(filepath.Join(".codex", "sessions", "2026", "01", "02", "rollout-2026-01-02T00-00-00-"+idB+".jsonl"), "{}\n")
	eq(t, ResolveTranscript("codex", idB, map[string]string{"HOME": f.home}), c, "codex")
	g := f.write(filepath.Join(".gemini", "antigravity-cli", "brain", idC, ".system_generated", "logs", "transcript.jsonl"), "{}\n")
	eq(t, ResolveTranscript("agy", idC, map[string]string{"HOME": f.home}), g, "agy")
	eq(t, codexHomeOfTranscript(c), filepath.Join(f.home, ".codex"), "codex home of rollout")
}

func TestClaudeFamily(t *testing.T) {
	f := newFixture(t)
	p := f.claudeTranscript("fam", "claude-fable-5-1", testNow)
	fam, err := ClaudeFamily(p)
	eq(t, err, error(nil), "err")
	eq(t, fam, "fable", "family")

	// Only the most recent assistant record counts, and only the tail is read.
	var b strings.Builder
	b.WriteString(`{"type":"assistant","message":{"model":"claude-opus-4"}}` + "\n")
	for i := 0; i < 300; i++ {
		b.WriteString(`{"type":"user","message":{"content":"` + strings.Repeat("x", 500) + `"}}` + "\n")
	}
	b.WriteString(`{"type":"assistant","message":{"model":"claude-sonnet-4-6"}}` + "\n")
	p2 := f.write("big.jsonl", b.String())
	fam, _ = ClaudeFamily(p2)
	eq(t, fam, "sonnet", "last record")

	p3 := f.write("old.jsonl", strings.TrimPrefix(b.String(), "")[:strings.LastIndex(b.String(), `{"type":"assistant"`)])
	_, err = ClaudeFamily(p3)
	if err == nil {
		t.Fatal("an assistant record beyond the last 200 lines must not count")
	}
}
