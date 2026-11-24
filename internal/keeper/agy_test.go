package keeper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// agyAgent creates an Antigravity agent: a Paseo provider whose command is
// agy, the agent's record and inspect answer, and an agy transcript with
// one model call of the given context.
func (h *harness) agyAgent(id, session string, tokens int64) string {
	cfg := paseo.Obj("agents", paseo.Obj("providers", paseo.Obj("agy-acp", paseo.Obj("extends", "acp",
		"command", []any{"/x/bin/agy", "acp"}, "env", paseo.Obj("AGY_ACCOUNT_ID", "cave")))))
	text, _ := paseo.DumpIndent(cfg, 2)
	if err := os.WriteFile(filepath.Join(h.home, "config.json"), []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.record(id, session, "idle", map[string]any{"provider": "agy-acp", "config": paseo.Obj("model", "gemini-4-neon-high")})
	h.paseo.inspects[id] = paseo.Obj("Id", id, "Name", "synthetic agy agent", "Provider", "agy-acp", "Model", "gemini-4-neon-high",
		"Cwd", "/tmp/example-project", "Status", "idle", "Archived", false, "PendingPermissions", []any{})
	dir := filepath.Join(h.user, ".gemini", "antigravity-cli", "brain", session, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	path := filepath.Join(dir, "transcript_full.jsonl")
	lines := jsonLine(map[string]any{"type": "USER_INPUT", "created_at": iso(h.now - 30), "content": "<USER_REQUEST>\nsynthetic task\n</USER_REQUEST>"}) + "\n" +
		jsonLine(map[string]any{"type": "PLANNER_RESPONSE", "created_at": iso(h.now), "input_tokens": 1000, "cache_read_tokens": tokens - 1000}) + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return path
}

// The keeper reads an agy agent's activity and context from agy's
// transcript; it compacts on policy but refuses to warm Gemini's cache.
func TestAgyAgentCompactsButIsNotWarmed(t *testing.T) {
	h := newHarness(t)
	h.agyAgent(agentA, sessionA, big)
	_, err := h.k.SetPolicy(context.Background(), agentA, h.home, PolicyWarm, 0, "cli")
	if !errors.Is(err, ErrBadRequest) || !contains(err.Error(), "best effort") {
		t.Fatalf("warm: %v", err)
	}
	h.set(agentA, PolicyCompact)
	v := h.view(agentA)
	if v.Provider != paseo.AgentAgy || v.LastActivitySource != SourceTranscript || v.ContextTokens == nil || *v.ContextTokens != big {
		t.Fatalf("view %+v", v)
	}
	if v.TTLSeconds != 3600 || v.NextAction.Kind != ActCompact || *v.NextAction.At != t0+3600-lead {
		t.Fatalf("plan %+v ttl %d", v.NextAction, v.TTLSeconds)
	}
}

// Paseo's timeline shows nothing of an agy compaction; the CHECKPOINT agy
// writes to its transcript completes it.
func TestAgyCompactionCompletesFromTranscript(t *testing.T) {
	h := newHarness(t)
	transcript := h.agyAgent(agentA, sessionA, small)
	if _, err := h.k.Compact(context.Background(), agentA, h.home); err != nil {
		t.Fatal(err)
	}
	h.wantSends(1)
	h.paseo.logs[agentA] = []*paseo.Object{paseo.Obj("type", "user_message", "text", "/compact")}
	sent := h.now
	h.now += 5
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(jsonLine(map[string]any{"type": "USER_INPUT", "created_at": iso(sent), "content": "<USER_REQUEST>\n/compact\n</USER_REQUEST>"}) + "\n")
	f.WriteString(jsonLine(map[string]any{"type": "CHECKPOINT", "created_at": iso(sent + 1), "input_tokens": 10, "cache_read_tokens": 900}) + "\n")
	f.Close()
	h.step(agentA)
	if c := h.cache(agentA).Compaction; c.State != StateCompleted {
		t.Fatalf("compaction %+v", c)
	}
}
