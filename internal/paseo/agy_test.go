package paseo

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Every id below is synthetic.

const agyQuotaText = "\n⚠ Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 2h19m26s.\n"

func TestAgyNotice(t *testing.T) {
	for text, want := range map[string]bool{
		agyQuotaText: true,
		"⚠ Individual quota reached.\n⚠ Individual quota reached.": true,
		"Done. The run hit your usage limit twice, see the log.":   false, // a report, not a notice
		"⚠ The build failed.\nquota reached later":                 false, // the first line decides
		"": false,
	} {
		if got := agyNotice(text); got != want {
			t.Errorf("%q: %v", text, got)
		}
	}
	for text, want := range map[string]time.Duration{
		agyQuotaText:                      2*time.Hour + 19*time.Minute + 26*time.Second,
		"⚠ quota reached. Resets in 45m":  45 * time.Minute,
		"⚠ quota reached. Resets in 1d2h": 26 * time.Hour,
		"⚠ quota reached.":                0,
	} {
		if got := agyResetIn(text); got != want {
			t.Errorf("%q: %v", text, got)
		}
	}
}

func TestAgyStopIn(t *testing.T) {
	reply := func(text, id string) *Object { return Obj("type", "assistant_message", "text", text, "messageId", id) }
	incoming := Obj("type", "tool_call", "callId", "paseo-agent-message:m1", "agentMessage", Obj("text", "status?"))
	own := Obj("type", "tool_call", "callId", "call_1", "name", "execute")
	cases := []struct {
		items []*Object
		id    string
	}{
		{[]*Object{own, reply(agyQuotaText, "a")}, "a"},
		// A message to the stopped agent started a turn that hit the limit again.
		{[]*Object{reply(agyQuotaText, "a"), incoming, reply(agyQuotaText, "b")}, "b"},
		// A message that has not been answered yet does not hide the stop.
		{[]*Object{reply(agyQuotaText, "a"), incoming, Obj("type", "user_message", "text", "go on")}, "a"},
		{[]*Object{reply(agyQuotaText, "a"), own}, ""},
		{[]*Object{reply(agyQuotaText, "a"), Obj("type", "reasoning", "text", "x")}, ""},
		{[]*Object{reply("All done.", "a")}, ""},
		{nil, ""},
	}
	for i, c := range cases {
		id, _, ok := agyStopIn(c.items)
		if id != c.id || ok != (c.id != "") {
			t.Errorf("case %d: %q %v", i, id, ok)
		}
	}
}

func TestAgyAppDir(t *testing.T) {
	user := realTemp(t)
	cases := []struct {
		argv []any
		env  *Object
		want string
	}{
		{[]any{"agy", "acp", "--gemini_dir=" + user + "/.driver/gemini", "--app_data_dir=../../.gemini/antigravity-cli"}, NewObject(),
			filepath.Join(user, ".gemini", "antigravity-cli")},
		{[]any{"agy", "acp", "--app_data_dir", "/abs/app"}, NewObject(), "/abs/app"},
		{[]any{"agy", "acp"}, Obj("ANTIGRAVITY_GEMINI_DIR", "~/g2"), filepath.Join(user, "g2", "antigravity-cli")},
		{nil, NewObject(), filepath.Join(user, ".gemini", "antigravity-cli")},
	}
	for i, c := range cases {
		if got := agyAppDir(c.argv, c.env, user); got != realpath(c.want) {
			t.Errorf("case %d: %s", i, got)
		}
	}
}

type tailingPaseo struct {
	fakePaseo
	items []*Object
	tails int
}

func (f *tailingPaseo) Tail(_ context.Context, agentID, home string, n int) ([]*Object, error) {
	f.tails++
	return f.items, nil
}

// A stop is read once per activity of the agent, and a closed agent's
// timeline is never read (Paseo would load the agent again to answer).
func TestAgyStopOfCachesAndSkipsClosed(t *testing.T) {
	h := newHarness(t)
	p := &tailingPaseo{items: []*Object{Obj("type", "assistant_message", "text", agyQuotaText, "messageId", "m1")}}
	h.e.Paseo = p
	record := Obj("id", "agy-agent", "lastStatus", "idle", "lastActivityAt", "2026-11-24T17:19:46Z")
	stop, err := h.e.AgyStopOf(record, h.home)
	if err != nil || stop.Failure == "" {
		t.Fatal(stop, err)
	}
	want := float64(time.Date(2026, 11, 24, 19, 39, 12, 0, time.UTC).Unix())
	if stop.ResetAt != want {
		t.Fatalf("reset %v, want %v", stop.ResetAt, want)
	}
	again, _ := h.e.AgyStopOf(record, h.home)
	record.Set("lastStatus", "closed") // closing does not move lastActivityAt
	closed, _ := h.e.AgyStopOf(record, h.home)
	if p.tails != 1 || again != stop || closed != stop {
		t.Fatalf("tails %d: %+v %+v", p.tails, again, closed)
	}
	record.Set("lastActivityAt", "2026-11-24T20:00:00Z")
	if s, _ := h.e.AgyStopOf(record, h.home); s.Failure != "" || p.tails != 1 {
		t.Fatalf("closed agent read: %+v, tails %d", s, p.tails)
	}
	record.Set("lastStatus", "idle")
	p.items = []*Object{Obj("type", "assistant_message", "text", "Back at work.", "messageId", "m2")}
	if s, _ := h.e.AgyStopOf(record, h.home); s.Failure != "" || p.tails != 2 {
		t.Fatalf("after new activity: %+v, tails %d", s, p.tails)
	}
}

func agyHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.provider = "agy-acp"
	h.config = Obj("agents", Obj("providers", Obj(h.provider, Obj("extends", "acp",
		"command", []any{"/x/bin/agy", "acp"}, "env", Obj("AGY_ACCOUNT_ID", "cave")))))
	h.writeConfig()
	h.snapshot = Obj("Id", "agent-id", "Provider", h.provider, "Cwd", "/tmp/project", "Model", "gemini-4-neon-high",
		"Status", "idle", "Archived", false, "PendingPermissions", []any{})
	h.paseo.snapshot = h.snapshot
	h.record = Obj("id", "agent-id", "lastStatus", "idle", "lastActivityAt", "2026-11-24T17:19:46Z")
	h.e.agyStopFn = func(*Object, string) (AgyStop, error) { return AgyStop{Failure: "agy-f1"}, nil }
	h.e.activityFn = func(*Object, Identity) (Activity, error) { return Activity{}, nil }
	h.state = Obj("enabled", true, "agent_id", "agent-id", "home", h.home, "binding", h.binding(h.snapshot), "poll", 300)
	h.write()
	return h
}

// An agy agent stopped by its quota notice is continued once its account
// has room for its model again.
func TestAgyAgentContinuesAfterQuota(t *testing.T) {
	h := agyHarness(t)
	if b := h.binding(h.snapshot); b.Value("agent") != AgentAgy || b.Value("profile") != "cave" {
		t.Fatal(b)
	}
	h.e.agyStopFn = func(*Object, string) (AgyStop, error) { return AgyStop{Failure: "agy-f1", ResetAt: 1795549152}, nil }
	h.quotas.usable = false
	if s := h.tick(); s != StatusWaitingQuota {
		t.Fatal(s)
	}
	// The scheduler checks again just after the reset the notice names.
	if at := epochOf(h.read().Value("wake_at")); at != 1795549152+agyWakeSlack {
		t.Fatalf("wake_at %v", at)
	}
	h.quotas.usable = true
	if s := h.tick(); !strings.Contains(s, "sent") {
		t.Fatal(s)
	}
	if _, ok := h.read().Get("wake_at"); ok {
		t.Fatal("wake_at outlived the wait")
	}
	if len(h.paseo.sent) != 1 || h.paseo.sent[0].text != ContinueText {
		t.Fatalf("%+v", h.paseo.sent)
	}
	want := quotaCall{"agy", "cave", "gemini-4-neon-high"}
	if h.quotas.calls[0] != want || h.quotas.calls[len(h.quotas.calls)-1] != want {
		t.Fatalf("%+v", h.quotas.calls)
	}
	if s := h.tick(); s != StatusWaitingNew || len(h.paseo.sent) != 1 {
		t.Fatal(s, len(h.paseo.sent))
	}
}

func TestStoppedAgentsAgy(t *testing.T) {
	h := agyHarness(t)
	writeRecords(t, filepath.Join(h.home, "agents", "-tmp-project"),
		Obj("id", "agy-stopped", "provider", h.provider, "lastStatus", "idle"),
		Obj("id", "agy-working", "provider", h.provider, "lastStatus", "idle"),
		Obj("id", "agy-builtin", "provider", "antigravity", "lastStatus", "closed"),
		Obj("id", "agy-running", "provider", h.provider, "lastStatus", "running"),
	)
	h.e.agyStopFn = func(r *Object, _ string) (AgyStop, error) {
		if id, _ := r.String("id"); id == "agy-stopped" || id == "agy-builtin" || id == "agy-running" {
			return AgyStop{Failure: "x"}, nil
		}
		return AgyStop{}, nil
	}
	found, err := h.e.Stopped(context.Background(), h.home, false, nil)
	sort.Strings(found)
	if err != nil || !reflect.DeepEqual(found, []string{"agy-builtin", "agy-stopped"}) {
		t.Fatal(found, err)
	}
}

func TestScanAgy(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"USER_INPUT","created_at":"2026-11-24T10:00:00Z","content":"<USER_REQUEST>\nfix it\n</USER_REQUEST>"}`),
		[]byte(`{"type":"PLANNER_RESPONSE","created_at":"2026-11-24T10:00:05Z","input_tokens":3000,"cache_read_tokens":200000}`),
		[]byte(`{"type":"USER_INPUT","created_at":"2026-11-24T11:00:00Z","content":"<USER_REQUEST>\n` + KeeperMarker + ` ping\n</USER_REQUEST>\n<ADDITIONAL_METADATA>x</ADDITIONAL_METADATA>"}`),
		[]byte(`{"type":"PLANNER_RESPONSE","created_at":"2026-11-24T11:00:03Z","input_tokens":10,"cache_read_tokens":203000}`),
	}
	a := scanAgy(lines)
	at := func(s string) float64 { return parseTime(s) }
	if !a.Found || a.RealAt != at("2026-11-24T10:00:05Z") || a.CacheAt != at("2026-11-24T11:00:03Z") ||
		!a.HasContext || a.ContextTokens != 203010 || !a.LastTurnKeeper {
		t.Fatalf("%+v", a)
	}
	lines = append(lines,
		[]byte(`{"type":"USER_INPUT","created_at":"2026-11-24T12:00:00Z","content":"<USER_REQUEST>\n/compact\n</USER_REQUEST>"}`),
		[]byte(`{"type":"CHECKPOINT","created_at":"2026-11-24T12:00:01Z","input_tokens":1738,"cache_read_tokens":55367}`))
	a = scanAgy(lines)
	if a.CompactedAt != at("2026-11-24T12:00:01Z") || a.HasContext || !a.LastTurnKeeper || a.RealAt != at("2026-11-24T10:00:05Z") {
		t.Fatalf("%+v", a)
	}
}
