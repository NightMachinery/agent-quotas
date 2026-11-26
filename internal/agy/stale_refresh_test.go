package agy

import (
	"context"
	"strings"
	"testing"
)

// The daemon re-reads an account whose newest reading is older than
// stale_refresh_s, whether or not anyone uses it; a failed run waits a full
// interval before the next try.
func TestStaleRefreshByAge(t *testing.T) {
	f := newFixture(t)
	f.cfg.Agy.ActiveRefreshS = -1
	f.cfg.Agy.StaleRefreshS = 3600
	f.reader.StaleRefresh = true
	ctx := context.Background()
	const newest = 1999990300 // the newest captured_at in testdata/m-cache.json

	if snap, _ := f.reader.Read(ctx, ref("m"), at(newest+3599)); snap.Source != "statusline" || f.agy.count() != 0 {
		t.Fatalf("younger than the interval: source %q, %d calls", snap.Source, f.agy.count())
	}
	f.agy.exit["m"] = 3
	if snap, _ := f.reader.Read(ctx, ref("m"), at(newest+3601)); snap.Source != "statusline" || f.agy.count() != 1 {
		t.Fatalf("failed refresh: source %q, %d calls; want the cache served after one try", snap.Source, f.agy.count())
	}
	if f.reader.Read(ctx, ref("m"), at(newest+3700)); f.agy.count() != 1 {
		t.Fatalf("retried %d times within the interval after a failure", f.agy.count()-1)
	}
	delete(f.agy.exit, "m")
	snap, err := f.reader.Read(ctx, ref("m"), at(newest+3601+3600))
	if err != nil || snap.Source != "slow" || f.agy.count() != 2 {
		t.Fatalf("an interval after the failure: source %q err %v, %d calls", snap.Source, err, f.agy.count())
	}
	if !strings.Contains(string(mustJSON(t, snap.Notes)), "reading older than 1h") {
		t.Errorf("notes %+v do not say why", snap.Notes)
	}
	if f.reader.Read(ctx, ref("m"), at(newest+3601+3700)); f.agy.count() != 2 {
		t.Fatalf("the refreshed reading was re-read again: %d calls", f.agy.count())
	}
}

// A pool whose reset has passed since it was read triggers one re-read,
// blocked_grace_s after the reset, even though the age alone would not.
func TestStaleRefreshAtReset(t *testing.T) {
	f := newFixture(t)
	f.cfg.Agy.ActiveRefreshS = -1
	f.cfg.Agy.StaleRefreshS = 86400
	f.cfg.Poll.BlockedGraceS = 90
	f.reader.StaleRefresh = true
	ctx := context.Background()
	const reset = 2000000000 // gemini-5h in testdata/m-cache.json, captured before it

	if f.reader.Read(ctx, ref("m"), at(reset+30)); f.agy.count() != 0 {
		t.Fatalf("re-read within the grace: %d calls", f.agy.count())
	}
	snap, err := f.reader.Read(ctx, ref("m"), at(reset+100))
	if err != nil || snap.Source != "slow" || f.agy.count() != 1 {
		t.Fatalf("after the grace: source %q err %v, %d calls", snap.Source, err, f.agy.count())
	}
	if !strings.Contains(string(mustJSON(t, snap.Notes)), "reset since it was read") {
		t.Errorf("notes %+v do not say why", snap.Notes)
	}
	// The fixture's /usage does not print gemini-5h, so its old reading and
	// passed reset stay in the cache; that reset has had its run.
	if f.reader.Read(ctx, ref("m"), at(reset+200)); f.agy.count() != 1 {
		t.Fatalf("the same reset triggered again: %d calls", f.agy.count())
	}
}

// Off unless the daemon asks for it, and off with stale_refresh_s -1.
func TestStaleRefreshOff(t *testing.T) {
	const old = 1999990300 + 7*86400
	for _, tc := range []struct {
		name   string
		daemon bool
		s      int
	}{{"not the daemon", false, 3600}, {"turned off", true, -1}} {
		f := newFixture(t)
		f.cfg.Agy.ActiveRefreshS = -1
		f.cfg.Agy.StaleRefreshS = tc.s
		f.reader.StaleRefresh = tc.daemon
		if snap, _ := f.reader.Read(context.Background(), ref("m"), at(old)); snap.Source != "statusline" || f.agy.count() != 0 {
			t.Errorf("%s: source %q, %d calls", tc.name, snap.Source, f.agy.count())
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
