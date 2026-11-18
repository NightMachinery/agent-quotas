package paseo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic transcripts only.

func line(v map[string]any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func ts(sec int) string {
	return time.Unix(1_800_000_000+int64(sec), 0).UTC().Format(time.RFC3339Nano)
}

func at(sec int) float64 { return float64(1_800_000_000 + sec) }

func cUser(sec int, text string) []byte {
	return line(map[string]any{"type": "user", "timestamp": ts(sec), "message": map[string]any{"content": text}})
}

func cAssistant(sec int, tokens int) []byte {
	return line(map[string]any{"type": "assistant", "timestamp": ts(sec), "message": map[string]any{
		"model": "claude-example", "usage": map[string]any{"input_tokens": 1, "cache_creation_input_tokens": 9, "cache_read_input_tokens": tokens - 10}}})
}

func TestIsKeeperPrompt(t *testing.T) {
	for text, want := range map[string]bool{
		KeeperMarker + " reply with one word": true,
		"  " + KeeperMarker:                   true,
		"/compact":                            true,
		"<command-name>/compact</command-name>\n<command-args></command-args>": true,
		"please " + KeeperMarker: false,
		"/compact-not":           false,
		"Continue.":              false,
	} {
		if got := IsKeeperPrompt(text); got != want {
			t.Errorf("IsKeeperPrompt(%q) = %v", text, got)
		}
	}
}

func TestScanClaudeSeparatesKeeperTurns(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"user","timestamp":` + `"cut in ha`), // a cut first line
		cUser(0, "real task"),
		line(map[string]any{"type": "user", "timestamp": ts(5), "message": map[string]any{
			"content": []any{map[string]any{"type": "tool_result", "content": "x"}}}}),
		cAssistant(10, 150000),
		cUser(100, KeeperMarker+" ping"),
		cAssistant(105, 151000),
		line(map[string]any{"type": "user", "timestamp": ts(106), "isMeta": true, "message": map[string]any{"content": "meta"}}),
	}
	a := scanClaude(lines)
	if a.RealAt != at(10) || a.CacheAt != at(105) || !a.HasContext || a.ContextTokens != 151000 || !a.LastTurnKeeper {
		t.Fatalf("%+v", a)
	}
	// A compaction turn: not real, refreshes the cache, context unknown.
	lines = append(lines, cUser(200, "<command-name>/compact</command-name>"),
		line(map[string]any{"type": "system", "subtype": "compact_boundary", "timestamp": ts(230)}))
	a = scanClaude(lines)
	if a.RealAt != at(10) || a.CacheAt != at(230) || a.HasContext || a.CompactedAt != at(230) {
		t.Fatalf("after compact %+v", a)
	}
	// A real turn whose call failed on the limit: the prompt is real
	// activity, the synthetic stop is not a model call.
	lines = append(lines, cUser(300, "next task"), line(map[string]any{"type": "assistant", "timestamp": ts(301),
		"isApiErrorMessage": true, "message": map[string]any{"model": "<synthetic>"}}))
	a = scanClaude(lines)
	if a.RealAt != at(300) || a.CacheAt != at(230) || a.LastTurnKeeper {
		t.Fatalf("after failed turn %+v", a)
	}
}

func cEvent(sec int, payload map[string]any) []byte {
	return line(map[string]any{"timestamp": ts(sec), "type": "event_msg", "payload": payload})
}

func TestScanCodexSeparatesKeeperTurns(t *testing.T) {
	tokens := func(sec, n int) []byte {
		return cEvent(sec, map[string]any{"type": "token_count", "info": map[string]any{
			"last_token_usage": map[string]any{"input_tokens": n}}})
	}
	lines := [][]byte{
		cEvent(0, map[string]any{"type": "task_started", "turn_id": "t1"}),
		cEvent(1, map[string]any{"type": "user_message", "message": "real task"}),
		tokens(20, 90000),
		cEvent(21, map[string]any{"type": "token_count", "info": nil}),
		cEvent(22, map[string]any{"type": "task_complete", "turn_id": "t1"}),
		cEvent(100, map[string]any{"type": "task_started", "turn_id": "t2"}),
		cEvent(101, map[string]any{"type": "user_message", "message": KeeperMarker + " ping"}),
		tokens(110, 91000),
	}
	a := scanCodex(lines)
	if a.RealAt != at(20) || a.CacheAt != at(110) || a.ContextTokens != 91000 || !a.LastTurnKeeper {
		t.Fatalf("%+v", a)
	}
	// A manual compaction turn has no prompt, only the compaction.
	lines = append(lines,
		cEvent(200, map[string]any{"type": "task_started", "turn_id": "t3"}),
		line(map[string]any{"timestamp": ts(210), "type": "compacted", "payload": map[string]any{}}),
		tokens(211, 20000))
	a = scanCodex(lines)
	if a.RealAt != at(20) || a.CacheAt != at(211) || a.CompactedAt != at(210) || !a.LastTurnKeeper {
		t.Fatalf("after compaction %+v", a)
	}
	lines = append(lines,
		cEvent(300, map[string]any{"type": "task_started", "turn_id": "t4"}),
		cEvent(301, map[string]any{"type": "user_message", "message": "more"}),
		cEvent(302, map[string]any{"type": "task_started", "turn_id": "sub", "root_turn_id": "t4"}),
		tokens(320, 25000))
	a = scanCodex(lines)
	if a.RealAt != at(320) || a.LastTurnKeeper {
		t.Fatalf("after real turn %+v", a)
	}
}

func TestActivityOfReadsTheSessionTranscript(t *testing.T) {
	root := realTemp(t)
	session := "22222222-2222-4222-8222-222222222222"
	dir := filepath.Join(root, "projects", "p")
	os.MkdirAll(dir, 0o700)
	data := strings.Join([]string{string(cUser(0, "task")), string(cAssistant(3, 1000))}, "\n") + "\n"
	os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(data), 0o600)
	record := Obj("persistence", Obj("sessionId", session))
	a, err := ActivityOf(record, Identity{Agent: "claude", NativeHome: root})
	if err != nil || !a.Found || a.RealAt != at(3) {
		t.Fatalf("%+v %v", a, err)
	}
	a, err = ActivityOf(Obj(), Identity{Agent: "claude", NativeHome: root})
	if err != nil || a.Found {
		t.Fatalf("no session: %+v %v", a, err)
	}
}

func TestKeeperPingTurnIsNotContinued(t *testing.T) {
	h := newHarness(t)
	h.e.activityFn = func(*Object, Identity) (Activity, error) {
		return Activity{Found: true, LastTurnKeeper: true}, nil
	}
	if s := h.tick(); s != StatusPingNotContinued {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("a ping turn was continued")
	}
	if claimed, _ := h.read().String("claimed_failure"); claimed == "" {
		t.Fatal("the ping turn was not claimed")
	}
	// A later real failed turn is continued as usual.
	h.e.activityFn = func(*Object, Identity) (Activity, error) { return Activity{Found: true}, nil }
	h.record.Set("lastUserMessageAt", "turn-2")
	if s := h.tick(); !strings.Contains(s, "sent") {
		t.Fatal(s)
	}
}

func TestKeeperPingTurnFallbackOnPingTime(t *testing.T) {
	h := newHarness(t)
	h.e.activityFn = func(*Object, Identity) (Activity, error) { return Activity{}, nil }
	h.record.Set("lastUserMessageAt", ts(5))
	h.state.Set("cache", Obj("policy", "warm", "last_ping", Obj("at", at(0), "ok", true, "error", "")))
	h.write()
	if s := h.tick(); s != StatusPingNotContinued {
		t.Fatal(s)
	}
	if len(h.paseo.sent) != 0 {
		t.Fatal("sent")
	}
}

func TestOnKeepsTheCacheObject(t *testing.T) {
	h := newHarness(t)
	agentID := "00000000-0000-4000-8000-000000000003"
	path := h.onFixture(agentID)
	ctx := context.Background()
	if _, err := h.e.On(ctx, agentID, h.home, 300); err != nil {
		t.Fatal(err)
	}
	state, _ := readObject(path)
	state.Set("cache", Obj("policy", "warm"))
	if err := atomicWrite(path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Off(ctx, agentID, h.home); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.On(ctx, agentID, h.home, 300); err != nil {
		t.Fatal(err)
	}
	state, _ = readObject(path)
	if c, ok := state.Object("cache"); !ok || c.Value("policy") != "warm" {
		t.Fatalf("cache lost: %v", state.Value("cache"))
	}
}
