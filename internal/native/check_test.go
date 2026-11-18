package native

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

const idD = "00000000-0000-4000-8000-00000000000d"

type checkFixture struct {
	*fixture
	sent []string
}

// newCheckFixture registers four claude-work sessions:
// A on Fable and working, B on Opus and working, C on Opus but idle for two
// hours, D with no transcript at all.
func newCheckFixture(t *testing.T) *checkFixture {
	f := &checkFixture{fixture: newFixture(t)}
	f.e.Deliver = func(ctx context.Context, target string) error {
		f.sent = append(f.sent, target)
		if strings.Contains(target, "gone") {
			return errors.New("gone")
		}
		return nil
	}
	ta := f.claudeTranscript(idA, "claude-fable-5", testNow.Add(-100*time.Second))
	tb := f.claudeTranscript(idB, "claude-opus-4-7", testNow.Add(-200*time.Second))
	tc := f.claudeTranscript(idC, "claude-opus-4-7", testNow.Add(-2*time.Hour))
	for _, r := range []struct{ id, tr, target string }{
		{idA, ta, "tmux:%1"}, {idB, tb, "kitty:2"}, {idC, tc, "kitty:3"}, {idD, "", "tmux:%4"},
	} {
		if _, err := f.e.Register(Here{Agent: "claude", ID: r.id, Transcript: r.tr, Scope: "claude-work"}, []string{r.target}); err != nil {
			t.Fatal(err)
		}
	}
	f.l.rows = []LiveRow{{ID: idA}, {ID: idB}, {ID: idC}, {ID: idD}}
	return f
}

func (f *checkFixture) check(q ScopeQuota) Decision {
	f.t.Helper()
	d, err := f.e.Check(context.Background(), "claude-work", q)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func fp(v float64) *float64 { return &v }

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func TestCheckAccountWideCycle(t *testing.T) {
	f := newCheckFixture(t)
	reset := float64(testNow.Unix() + 3600)
	blocked := ScopeQuota{Verdict: model.Blocked, BlockedUntil: fp(reset)}

	d := f.check(blocked)
	eq(t, d.Outcome, OutcomeBlocked, "first blocked check")
	eq(t, *d.BlockedSince, testNow.Unix(), "reference is the first sighting")
	eq(t, d.Account, "claude:work", "account")

	f.now = testNow.Add(10 * time.Minute)
	d = f.check(blocked)
	eq(t, *d.BlockedSince, testNow.Unix(), "reference stays put")

	// Usable, but within the grace after the reset.
	f.now = time.Unix(int64(reset)+60, 0)
	d = f.check(ScopeQuota{Verdict: model.Usable})
	eq(t, d.Outcome, OutcomeWaiting, "inside the grace")
	eq(t, *d.RetryAt, reset+120, "retry at the end of the grace")
	eq(t, len(f.sent), 0, "nothing sent yet")

	f.now = time.Unix(int64(reset)+130, 0)
	d = f.check(ScopeQuota{Verdict: model.Usable})
	eq(t, d.Outcome, OutcomeResumed, "resumed")
	eqSlice(t, sortedCopy(f.sent), []string{"kitty:2", "tmux:%1", "tmux:%4"}, "the working sessions, and the one with no transcript")
	eqSlice(t, d.Skipped, []string{"claude-" + idC}, "the idle session is skipped")
	var labels []string
	for _, r := range d.Fires {
		labels = append(labels, r.Label)
		if !strings.HasPrefix(r.Report, "Claude Code (work): quota reset, usage available again -- resumed ") {
			t.Fatalf("report: %q", r.Report)
		}
	}
	eqSlice(t, labels, []string{"agent-auto-continue-claude-work", "agent-auto-continue-claude-work-fable", "agent-auto-continue-claude-work-opus"}, "one fire per family group")
	eq(t, len(f.n.bodies), 3, "one notification per fire")
	lines := f.logLines()
	eq(t, len(lines), 3, "one log line per fire")
	if !strings.Contains(lines[0], " agent-auto-continue-claude-work: Claude Code (work): quota reset") {
		t.Fatalf("log line: %q", lines[0])
	}
	eq(t, f.exists(filepath.Join(f.e.scopeDir("claude-work"), ".blocked_at")), false, "cycle cleared")
	eq(t, f.exists(filepath.Join(f.e.scopeDir("claude-work"), ".blocked-state")), false, "state cleared")

	f.sent = nil
	d = f.check(ScopeQuota{Verdict: model.Usable})
	eq(t, d.Outcome, OutcomeIdle, "idle afterwards")
	eq(t, len(f.sent), 0, "no second resume")
}

func TestCheckFamilyOnly(t *testing.T) {
	f := newCheckFixture(t)
	reset := float64(testNow.Unix() + 3600)
	d := f.check(ScopeQuota{Verdict: model.Usable, ModelBlocks: []ModelBlock{{Name: "Fable weekly", ResetsAt: fp(reset)}}})
	eq(t, d.Outcome, OutcomeBlocked, "a family block is a block")

	f.now = time.Unix(int64(reset)+200, 0)
	d = f.check(ScopeQuota{Verdict: model.Usable})
	eq(t, d.Outcome, OutcomeResumed, "resumed")
	eqSlice(t, f.sent, []string{"tmux:%1"}, "only the Fable session")
	eq(t, d.Fires[0].Label, "agent-auto-continue-claude-work-fable", "label")
	eq(t, d.Fires[0].Message, "Claude Code (work): fable weekly reset, usage available again", "message")
}

func TestCheckAccountThenFamily(t *testing.T) {
	f := newCheckFixture(t)
	acct := float64(testNow.Unix() + 3600)
	week := float64(testNow.Unix() + 5*86400)
	f.check(ScopeQuota{Verdict: model.Blocked, BlockedUntil: fp(acct), ModelBlocks: []ModelBlock{{Name: "Claude Fable weekly", ResetsAt: fp(week)}}})

	// The 5h window resets; Fable's weekly window still blocks.
	f.now = time.Unix(int64(acct)+300, 0)
	d := f.check(ScopeQuota{Verdict: model.Usable, ModelBlocks: []ModelBlock{{Name: "Claude Fable weekly", ResetsAt: fp(week)}}})
	eq(t, d.Outcome, OutcomeResumed, "the others resume")
	eqSlice(t, sortedCopy(f.sent), []string{"kitty:2", "tmux:%4"}, "not the Fable session")
	eq(t, *d.BlockedSince, testNow.Unix(), "the reference stays for the Fable wait")

	f.sent = nil
	f.now = time.Unix(int64(week)+300, 0)
	d = f.check(ScopeQuota{Verdict: model.Usable})
	eqSlice(t, f.sent, []string{"tmux:%1"}, "Fable resumes at its own reset")
	eq(t, d.BlockedSince == nil, true, "cycle over")
}

func TestCheckHookMarkMakesIdleSessionEligible(t *testing.T) {
	f := newCheckFixture(t)
	f.check(ScopeQuota{Verdict: model.Blocked})
	res, err := f.e.Hook(context.Background(), "claude", "%9", []byte(`{"session_id":"`+idC+`","error":"rate_limit"}`))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Scope, "claude-work", "kick returns the scope")
	eq(t, res.Marked, true, "marked")
	d := f.check(ScopeQuota{Verdict: model.Usable})
	eq(t, len(d.Skipped), 0, "the marked session is eligible")
	eqSlice(t, sortedCopy(f.sent), []string{"kitty:2", "kitty:3", "tmux:%1", "tmux:%4"}, "all four")
	eq(t, f.e.marked("claude-work", "claude", idC), false, "mark cleared")
}

func TestCheckPrunes(t *testing.T) {
	f := newCheckFixture(t)
	f.l.err = errors.New("listing broke")
	d := f.check(ScopeQuota{Verdict: model.Usable})
	eq(t, len(d.Pruned), 0, "a failed listing prunes nothing")
	eq(t, len(d.Kept), 4, "kept")

	f.l.err = nil
	tb, _ := f.e.Registrations("claude-work")
	f.l.rows = []LiveRow{{ID: idA}, {Transcript: tb[1].Transcript}}
	d = f.check(ScopeQuota{Verdict: model.Usable})
	eqSlice(t, d.Pruned, []string{"claude-" + idC, "claude-" + idD}, "pruned")
	eqSlice(t, d.Kept, []string{"claude-" + idA, "claude-" + idB}, "kept by id or by transcript")
	eq(t, f.exists(f.e.regFile("claude-work", "claude", idC)), false, "file removed")

	f.l.rows = nil
	d = f.check(ScopeQuota{Verdict: model.Blocked})
	eq(t, d.Outcome, OutcomeEmpty, "everything pruned")
}

func TestCheckUnknownChangesNothing(t *testing.T) {
	f := newCheckFixture(t)
	d := f.check(ScopeQuota{Verdict: model.Unknown})
	eq(t, d.Outcome, OutcomeUnknown, "unknown")
	eq(t, f.exists(filepath.Join(f.e.scopeDir("claude-work"), ".blocked_at")), false, "no reference")
	f.check(ScopeQuota{Verdict: model.Blocked})
	d = f.check(ScopeQuota{Verdict: model.SignedOut})
	eq(t, d.Outcome, OutcomeUnknown, "signed out decides nothing")
	eq(t, len(f.sent), 0, "nothing sent")
	eq(t, f.exists(filepath.Join(f.e.scopeDir("claude-work"), ".blocked_at")), true, "cycle kept")
}

// A cycle the shell watcher recorded (.blocked_at only) is honoured: the
// scope counts as blocked as a whole, with no reset time to wait for.
func TestCheckShellRecordedCycle(t *testing.T) {
	f := newCheckFixture(t)
	f.write(filepath.Join(f.e.scopeDir("claude-work"), ".blocked_at"), "1800000000\n")
	d := f.check(ScopeQuota{Verdict: model.Usable})
	eq(t, d.Outcome, OutcomeResumed, "resumed")
	eqSlice(t, sortedCopy(f.sent), []string{"kitty:2", "tmux:%1", "tmux:%4"}, "eligible ones")
}

func TestCheckCodexScope(t *testing.T) {
	f := newFixture(t)
	var sent []string
	f.e.Deliver = func(ctx context.Context, target string) error { sent = append(sent, target); return nil }
	if _, err := f.e.Register(Here{Agent: "codex", ID: idA, Scope: "codex-alpha"}, []string{"codex:" + idA}); err != nil {
		t.Fatal(err)
	}
	f.l.rows = []LiveRow{{ID: idA}}
	// Model blocks never apply outside Claude.
	d, _ := f.e.Check(context.Background(), "codex-alpha", ScopeQuota{Verdict: model.Usable, ModelBlocks: []ModelBlock{{Name: "x"}}})
	eq(t, d.Outcome, OutcomeIdle, "codex usable")
	f.e.Check(context.Background(), "codex-alpha", ScopeQuota{Verdict: model.Blocked})
	d, _ = f.e.Check(context.Background(), "codex-alpha", ScopeQuota{Verdict: model.Usable})
	eqSlice(t, sent, []string{"codex:" + idA}, "codex resumed")
	eq(t, d.Fires[0].Message, "Codex (alpha): quota reset, usage available again", "message")
	eq(t, d.Account, "codex:alpha", "account")
}

func TestQuotaFromSnapshot(t *testing.T) {
	s := model.Snapshot{Verdict: model.Usable, Windows: []model.Window{
		{Key: "session", Scope: model.ScopeAccount, Blocking: false},
		{Key: "weekly_scoped", Family: "Fable", Label: "Weekly (Fable)", Scope: model.ScopeModel, Blocking: true, ResetsAt: fp(5)},
		{Key: "seven_day_opus", Family: "Opus", Scope: model.ScopeModel, Blocking: false},
	}}
	q := QuotaFromSnapshot(s)
	eq(t, len(q.ModelBlocks), 1, "one block")
	ok, r := q.familyBlocked("fable")
	eq(t, ok, true, "fable blocked")
	eq(t, r, 5.0, "reset")
	ok, _ = q.familyBlocked("opus")
	eq(t, ok, false, "opus not blocked")
	ok, _ = q.familyBlocked("")
	eq(t, ok, false, "no family never matches")
}
