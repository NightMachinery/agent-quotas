package agy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// agy run without a statusline (Paseo's `agy acp`, `agy -p`) spends quota
// but never writes the cache; activity newer than the cache's newest capture
// makes a normal read go live, at most once per active_refresh_s.
func TestUseSinceTheCacheRefreshes(t *testing.T) {
	f := newFixture(t)
	f.cfg.Agy.ActiveRefreshS = 300
	ctx := context.Background()
	const newest = 1999990300 // the newest captured_at in testdata/m-cache.json
	a := ResolveAccount(f.cfg.Agy.Accounts[indexOf(f.cfg, "m")])
	used := func(epoch int64) {
		t.Helper()
		dir := filepath.Join(a.CLIDir(), "conversations")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		ts := time.Unix(epoch, 0)
		if err := os.Chtimes(dir, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	// Used before the newest capture: the cache stands.
	used(newest - 100)
	if snap, err := f.reader.Read(ctx, ref("m"), at(newest+1000)); err != nil || snap.Source != "statusline" {
		t.Fatalf("source %q err %v, want the cache", snap.Source, err)
	}
	// Used after it, but the cache is younger than the interval: it stands.
	used(newest + 50)
	if snap, _ := f.reader.Read(ctx, ref("m"), at(newest+100)); snap.Source != "statusline" || len(f.agy.calls) != 0 {
		t.Fatalf("source %q after %d calls, want the cache", snap.Source, len(f.agy.calls))
	}
	// Used after it, and the cache is older than the interval: live.
	snap, err := f.reader.Read(ctx, ref("m"), at(newest+1000))
	if err != nil || snap.Source != "slow" || len(f.agy.calls) != 1 {
		t.Fatalf("source %q err %v calls %d, want one live read", snap.Source, err, len(f.agy.calls))
	}
	// The live read rewrote the cache, so the next read serves it.
	if snap, _ := f.reader.Read(ctx, ref("m"), at(newest+1010)); snap.Source != "statusline" || len(f.agy.calls) != 1 {
		t.Fatalf("source %q calls %d, want the refreshed cache", snap.Source, len(f.agy.calls))
	}
}

// `agy -p /usage` touches the account's conversations while it runs. That
// touch is the slow path's own, not use: the reading it wrote is stamped
// after it, so the next reads serve the cache instead of starting agy every
// active_refresh_s.
func TestSlowRunIsNotUse(t *testing.T) {
	f := newFixture(t)
	f.cfg.Agy.ActiveRefreshS = 300
	a := ResolveAccount(f.cfg.Agy.Accounts[indexOf(f.cfg, "m")])
	dir := filepath.Join(a.CLIDir(), "conversations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const start = 2100000000
	f.agy.onRun = func(procx.Cmd) {
		ts := time.Unix(start, 500_000_000) // during the run, after it began
		os.Chtimes(dir, ts, ts)
	}
	ctx := context.Background()
	if snap, err := f.reader.Read(ctx, ref("m"), model.ReadOptions{Force: true, Now: func() time.Time { return time.Unix(start, 0) }}); err != nil || snap.Source != "slow" {
		t.Fatalf("forced read: source %q err %v", snap.Source, err)
	}
	for _, later := range []int64{start + 301, start + 3000} {
		if snap, _ := f.reader.Read(ctx, ref("m"), at(later)); snap.Source != "statusline" || f.agy.count() != 1 {
			t.Fatalf("at +%ds: source %q after %d calls, want the cache", later-start, snap.Source, f.agy.count())
		}
	}
}

// cave's conversations link to m's, so cave's slow run touches them. That
// is the reader's own run, not use of m.
func TestAnotherAccountsRunIsNotUse(t *testing.T) {
	f := newFixture(t)
	f.cfg.Agy.ActiveRefreshS = 300
	m := ResolveAccount(f.cfg.Agy.Accounts[indexOf(f.cfg, "m")])
	cave := ResolveAccount(f.cfg.Agy.Accounts[indexOf(f.cfg, "cave")])
	shared := filepath.Join(m.CLIDir(), "conversations")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(cave.CLIDir(), "conversations")); err != nil {
		t.Fatal(err)
	}
	const newest = 1999990300 // m's newest capture
	const run = newest + 1000
	f.agy.onRun = func(c procx.Cmd) {
		if envValue(c.Env, "AGY_ACCOUNT_ID") == "cave" {
			ts := time.Unix(run, 0)
			os.Chtimes(shared, ts, ts)
		}
	}
	ctx := context.Background()
	if _, err := f.reader.Read(ctx, ref("cave"), model.ReadOptions{Force: true, Now: func() time.Time { return time.Unix(run, 0) }}); err != nil {
		t.Fatal(err)
	}
	if snap, _ := f.reader.Read(ctx, ref("m"), at(run+400)); snap.Source != "statusline" || f.agy.count() != 1 {
		t.Fatalf("m after cave's run: source %q, %d calls; want the cache", snap.Source, f.agy.count())
	}
}

func TestActiveRefreshOff(t *testing.T) {
	f := newFixture(t)
	f.cfg.Agy.ActiveRefreshS = -1
	a := ResolveAccount(f.cfg.Agy.Accounts[indexOf(f.cfg, "m")])
	if err := os.MkdirAll(filepath.Join(a.CLIDir(), "conversations"), 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(2100000000, 0)
	os.Chtimes(filepath.Join(a.CLIDir(), "conversations"), ts, ts)
	if snap, _ := f.reader.Read(context.Background(), ref("m"), at(2100000100)); snap.Source != "statusline" || len(f.agy.calls) != 0 {
		t.Fatalf("source %q calls %d, want the cache", snap.Source, len(f.agy.calls))
	}
}

// A second account whose conversations link to the main account's does not
// count the main account's turns as its own.
func TestSymlinkedConversationsAreNotUse(t *testing.T) {
	f := newFixture(t)
	m := ResolveAccount(f.cfg.Agy.Accounts[indexOf(f.cfg, "m")])
	cave := ResolveAccount(f.cfg.Agy.Accounts[indexOf(f.cfg, "cave")])
	shared := filepath.Join(m.CLIDir(), "conversations")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(cave.CLIDir(), "conversations")); err != nil {
		t.Fatal(err)
	}
	if !m.LastUsed().After(time.Time{}) {
		t.Fatal("m's own conversations should count")
	}
	if got := cave.LastUsed(); !got.IsZero() {
		t.Fatalf("cave LastUsed %v through a symlink, want zero", got)
	}
}

// A model's pool that /usage printed before and leaves out now is retired:
// it leaves the cache and the report. A pool only the statusline hook
// reports stays, and so do the general pools.
func TestRetiredPoolLeaves(t *testing.T) {
	f := newFixture(t)
	f.agy.out = []byte("Gemini Models\tWeekly Limit Remaining\t91%\t2030-01-01T00:00:00Z\n" +
		"Claude and GPT models\tWeekly Limit Remaining\t6%\t2030-01-02T03:04:05Z\n")
	snap, err := f.reader.Read(context.Background(), ref("m"), model.ReadOptions{Force: true, Now: func() time.Time { return time.Unix(2000000100, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, w := range snap.Windows {
		keys[w.Key] = true
	}
	if keys["gemini-4-argon-weekly"] || !keys["gemini-4-argon-5h"] || !keys["gemini-5h"] || !keys["3p-weekly"] {
		t.Fatalf("report windows %v: want the slow-written Argon weekly gone, the hook's Argon 5h and the general pools kept", keys)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "main-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseRawObject(data)
	if err != nil {
		t.Fatal(err)
	}
	b := bucketsOf(doc)
	if b.has("gemini-4-argon-weekly") || !b.has("gemini-4-argon-5h") || !b.has("gemini-5h") {
		t.Fatalf("cache buckets after the run: %s", data)
	}
}
