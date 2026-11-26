package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	pos, err := parseInterspersed(fs, []string{"claude:work", "--json", "codex", "--", "--x"})
	if err != nil || !*j || !reflect.DeepEqual(pos, []string{"claude:work", "codex", "--x"}) {
		t.Fatalf("pos %v json %v err %v", pos, *j, err)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		85 * time.Minute:                "1h 25m",
		(4*24 + 7) * time.Hour:          "4d 7h",
		12 * time.Minute:                "12m",
		2 * time.Hour:                   "2h",
		40 * time.Second:                "40s",
		-time.Minute:                    "0s",
		3*24*time.Hour + 20*time.Minute: "3d",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestWriteStatus(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := model.Unix(now.Add(85 * time.Minute))
	doc := model.Document{Accounts: []model.Snapshot{{
		ID: "claude:work", Name: "work", Verdict: model.Blocked, Source: "api",
		FetchedAt: model.F(model.Unix(now.Add(-2 * time.Minute))), BlockedUntil: model.F(reset),
		Windows: []model.Window{{Family: "Claude", Window: "5h", UsedPercent: model.F(100), ResetsAt: model.F(reset), Blocking: true}},
		Notes:   []model.Note{{Severity: model.Warning, Text: "careful"}},
	}}}
	var b bytes.Buffer
	writeStatus(&b, doc, now)
	out := b.String()
	for _, want := range []string{"claude:work  blocked", "api 2m ago", "(in 1h 25m)", "! Claude", "100%  resets in 1h 25m", "warning: careful"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}

// A window read long before the account's newest reading says its own age;
// the others do not.
func TestWriteStatusOldWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fresh, old := model.Unix(now.Add(-3*time.Minute)), model.Unix(now.Add(-62*time.Hour))
	doc := model.Document{Accounts: []model.Snapshot{{
		ID: "agy:m", Name: "m", Verdict: model.Usable, Source: "statusline", FetchedAt: model.F(fresh),
		Windows: []model.Window{
			{Family: "Gemini", Window: "weekly", UsedPercent: model.F(0), CapturedAt: model.F(fresh)},
			{Family: "Gemini 9", Window: "weekly", UsedPercent: model.F(0), CapturedAt: model.F(old)},
		},
	}}}
	var b bytes.Buffer
	writeStatus(&b, doc, now)
	lines := strings.Split(b.String(), "\n")
	if !strings.Contains(lines[0], "statusline 3m ago") || strings.Contains(lines[1], "read ") || !strings.HasSuffix(lines[2], "read 2d 14h ago") {
		t.Fatalf("status:\n%s", b.String())
	}
}

func TestQuotaAdapterCodexAccount(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "codex")
	other := filepath.Join(root, "codex2")
	for _, d := range []string{def, other} {
		os.MkdirAll(d, 0o700)
	}
	os.WriteFile(filepath.Join(def, "auth_alpha.json"), []byte(`{"a":1}`), 0o600)
	os.WriteFile(filepath.Join(def, "auth.json"), []byte(`{"a":1}`), 0o600)
	os.WriteFile(filepath.Join(other, "auth.json"), []byte(`{"b":2}`), 0o600)
	cfg := &config.Config{Codex: config.Codex{Homes: []config.CodexHome{{Name: "default", CodexHome: def}, {Name: "second", CodexHome: other}}}}
	q := quotaAdapter{cfg: cfg}
	if id, err := q.codexAccount(def); err != nil || id != "codex:alpha" {
		t.Fatalf("default home: %q %v", id, err)
	}
	if id, err := q.codexAccount(other + "/"); err != nil || id != "codex:second/auth" {
		t.Fatalf("second home: %q %v", id, err)
	}
	if _, err := q.codexAccount(filepath.Join(root, "nope")); err == nil {
		t.Fatal("unconfigured home accepted")
	}
}

func TestQuotaAdapterPostponesOnStaleReading(t *testing.T) {
	stale := model.Snapshot{ID: "claude:work", Verdict: model.Usable, Stale: true, Error: model.S("429")}
	q := quotaAdapter{snap: func(context.Context, string, time.Duration) (model.Snapshot, error) { return stale, nil }}
	if _, err := q.Claude(context.Background(), "work", "opus"); err == nil {
		t.Fatal("a stale reading answered as usable")
	}
}

func TestBoundAccount(t *testing.T) {
	if got := boundAccount(paseo.Registration{Agent: "claude", Profile: "work"}); got != "claude:work" {
		t.Fatal(got)
	}
	if got := boundAccount(paseo.Registration{Agent: "codex", Profile: "codex"}); got != "codex" {
		t.Fatal(got)
	}
}
