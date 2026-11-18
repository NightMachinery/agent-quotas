package claude

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

func TestCloudCreditSpentOrExpiredIsHiddenUnlessAsked(t *testing.T) {
	rc := newRenderCtx(RenderOptions{Now: time.Unix(testNow, 0).UTC()})
	for _, o := range []string{`{"remaining_dollars":0,"used_dollars":250}`, `{"resets_at":1799999940}`} {
		c := extractCloudCredit(creditPayload(t, o), testNow, time.UTC)
		if c.Available {
			t.Errorf("%s: available", o)
		}
		if rc.cloudCredit(c, false) != "" {
			t.Errorf("%s: shown", o)
		}
		if !strings.Contains(rc.cloudCredit(c, true), "Cloud credit: $") {
			t.Errorf("%s: not shown when asked", o)
		}
	}
	if rc.cloudCredit(nil, true) != "" {
		t.Error("nil credit shown")
	}
}

func TestTextwrap(t *testing.T) {
	got := textwrap("[stale cache: aaaa bbbb cccccccccccccccccccc dd]", 20, "  ")
	want := []string{"  [stale cache: aaaa", "  bbbb", "  cccccccccccccccccccc", "  dd]"} // from Python
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%q", got)
	}
}

func TestLegacyJSONAndExitCode(t *testing.T) {
	ok := json.RawMessage(`{"profile":"a","error":null}`)
	bad := json.RawMessage(`{"profile":"b","error":"x"}`)
	if got := string(LegacyJSON([]json.RawMessage{ok}, false)); got != "{\n  \"profile\": \"a\",\n  \"error\": null\n}\n" {
		t.Errorf("single: %q", got)
	}
	if got := string(LegacyJSON([]json.RawMessage{ok, bad}, true)); !strings.HasPrefix(got, "[\n  {\n    \"profile\": \"a\"") {
		t.Errorf("array: %q", got)
	}
	if LegacyExitCode([]json.RawMessage{ok}) != 0 || LegacyExitCode([]json.RawMessage{ok, bad}) != 1 {
		t.Error("exit code")
	}
	if got := LegacyRow(model.Snapshot{Name: "w", Error: model.S("gone")}, false); string(got) != `{"profile":"w","error":"gone","keychain":{"service":"","account":""},"relogin":null}` {
		t.Errorf("synthesized row %s", got)
	}
}
