package main

import (
	"encoding/json"
	"testing"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

func TestClaudeOverage(t *testing.T) {
	snap := func(blocking bool, native string) model.Snapshot {
		return model.Snapshot{Provider: model.Claude, Native: json.RawMessage(native),
			Windows: []model.Window{{Key: "session", Scope: model.ScopeAccount, Blocking: blocking}}}
	}
	cases := []struct {
		name string
		s    model.Snapshot
		want bool
	}{
		{"exhausted on extra usage", snap(true, `{"extra_usage": {"is_enabled": true, "used_credits": 3}}`), true},
		{"exhausted, extra usage off", snap(true, `{"extra_usage": {"is_enabled": false}}`), false},
		{"exhausted, spend limit reached", snap(true, `{"extra_usage": {"is_enabled": true, "spend_limit_reached": true}}`), false},
		{"room left", snap(false, `{"extra_usage": {"is_enabled": true}}`), false},
		{"no extra usage key", snap(true, `{}`), false},
		{"codex", model.Snapshot{Provider: model.Codex}, false},
	}
	for _, tc := range cases {
		if got := claudeOverage(tc.s); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}
