package agy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
