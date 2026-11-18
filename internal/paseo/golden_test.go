package paseo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
)

// realTemp is t.TempDir with symlinks resolved (macOS's /var is /private/var).
func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProviderIdentitiesMatchPython(t *testing.T) {
	root := realTemp(t)
	user := filepath.Join(root, "user")
	for _, d := range []string{".claude", ".claude-work"} {
		if err := os.MkdirAll(filepath.Join(user, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(root, "daemon")
	copyFile(t, filepath.Join("testdata", "providers-config.json"), filepath.Join(home, "config.json"))
	cfg := &config.Config{Claude: config.Claude{Profiles: []config.ClaudeProfile{{Name: "default"}}}}
	e := &Engine{Config: cfg, UserHome: user}

	var rows []struct {
		Provider   string             `json:"provider"`
		Identity   *map[string]string `json:"identity"`
		Error      string             `json:"error"`
		AgyAccount *string            `json:"agy_account"`
	}
	readGolden(t, "providers-golden.json", &rows)
	checked := 0
	for _, row := range rows {
		switch {
		case row.Identity != nil:
			id, err := e.providerConfig(home, row.Provider)
			if err != nil {
				t.Errorf("%s: %v", row.Provider, err)
				continue
			}
			want := *row.Identity
			native := strings.Replace(id.NativeHome, user, "~", 1)
			if id.Agent != want["agent"] || id.Profile != want["profile"] || native != want["native_home"] || id.ConfigHash != want["config_hash"] {
				t.Errorf("%s: got %+v (native %s), want %v", row.Provider, id, native, want)
			}
			checked++
		case row.Error != "":
			if _, err := e.providerConfig(home, row.Provider); err == nil || err.Error() != row.Error {
				t.Errorf("%s: error %v, want %q", row.Provider, err, row.Error)
			}
			checked++
		default:
			account, ok, err := agyAccount(home, row.Provider)
			if err != nil {
				t.Fatal(err)
			}
			if (row.AgyAccount == nil) == ok || (ok && account != *row.AgyAccount) {
				t.Errorf("agy %s: got %q %v, want %v", row.Provider, account, ok, row.AgyAccount)
			}
			checked++
		}
	}
	if checked != len(rows) || checked < 15 {
		t.Fatalf("checked %d of %d", checked, len(rows))
	}
}

func TestFailureDigestsMatchPython(t *testing.T) {
	var want map[string]string
	readGolden(t, "failures-golden.json", &want)
	root := realTemp(t)

	session := "00000000-0000-4000-8000-0000000000aa"
	claudeHome := filepath.Join(root, "claude")
	copyFile(t, filepath.Join("testdata", "claude-transcript.jsonl"),
		filepath.Join(claudeHome, "projects", "-tmp-project", session+".jsonl"))
	got, err := claudeLimitFailure(Obj("persistence", Obj("sessionId", session)), claudeHome, tailBytes)
	if err != nil || got != want["claude"] {
		t.Errorf("claude: %q %v, want %q", got, err, want["claude"])
	}

	thread := "01a1bbc1-518f-7bb0-b332-515f9a0527ee"
	codexHome := filepath.Join(root, "codex")
	day := time.UnixMilli(0x01a1bbc1518f).Local().Format("2006/01/02")
	copyFile(t, filepath.Join("testdata", "codex-rollout.jsonl"),
		filepath.Join(codexHome, "sessions", day, "rollout-2026-01-01T00-00-00-"+thread+".jsonl"))
	record := Obj("persistence", Obj("sessionId", thread, "metadata", Obj("threadId", thread)))
	got, err = codexLimitFailure(record, codexHome, tailBytes)
	if err != nil || got != want["codex"] {
		t.Errorf("codex: %q %v, want %q", got, err, want["codex"])
	}

	rec, err := DecodeObject([]byte(`{"lastStatus": "error", "lastError": "You’ve hit your usage limit",
		"lastUserMessageAt": "2026-11-01T00:00:00.000Z", "attentionTimestamp": 1789000000123}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := quotaFailure(rec); got != want["last_error"] {
		t.Errorf("last_error: %q, want %q", got, want["last_error"])
	}
}
