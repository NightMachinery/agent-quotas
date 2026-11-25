package agy

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func TestFlatJSON(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	snap, err := f.reader.Read(ctx, ref("m"), at(1999990100))
	if err != nil {
		t.Fatal(err)
	}
	out, err := FlatJSON(snap.Native)
	if err != nil {
		t.Fatal(err)
	}
	var flat []map[string]any
	if err := json.Unmarshal(out, &flat); err != nil || len(flat) == 0 {
		t.Fatalf("%v: %s", err, out)
	}
	var keys []string
	for k := range flat[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"bucket_id", "group", "label", "remaining_percent", "resets_at", "resets_at_iso", "source"}
	if !reflect.DeepEqual(keys, want) || flat[0]["source"] != "cache" {
		t.Errorf("flat bucket %v", flat[0])
	}
	// No sign-in, or a failed read, has no array to give.
	for _, name := range []string{"fm2", "broke"} {
		snap, _ := f.reader.Read(ctx, ref(name), at(1999990100))
		if _, err := FlatJSON(snap.Native); err == nil {
			t.Errorf("%s: flat array without buckets", name)
		}
	}
}

func TestCacheOnlyForcedReadNeverRunsAgy(t *testing.T) {
	f := newFixture(t)
	f.reader.CacheOnly = true
	opts := at(1999990100)
	opts.Force = true
	if _, err := f.reader.Read(context.Background(), ref("m"), opts); err == nil || f.agy.count() != 0 {
		t.Errorf("deus under cache-only: %v, %d agy runs", err, f.agy.count())
	}
}

func TestCredits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.agy.out = []byte("Remaining credits\t0\nUpgrade\thttps://example.test/upgrade\n\nA banner line\n")
	rows, err := f.reader.Credits(ctx, "cave")
	want := []CreditRow{{"Remaining credits", "0"}, {"Upgrade", "https://example.test/upgrade"}, {"", "A banner line"}}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("credits %v %v", rows, err)
	}
	if argv := f.agy.calls[0].Argv; argv[len(argv)-1] != "/credits" || envValue(f.agy.calls[0].Env, "SSH_CONNECTION") == "" {
		t.Errorf("argv %q", argv)
	}
	// Signed out, cache-only and an unknown account never start agy.
	if _, err := f.reader.Credits(ctx, "fm2"); err == nil {
		t.Error("credits for an account with no sign-in")
	}
	if _, err := f.reader.Credits(ctx, "nobody"); err == nil {
		t.Error("credits for an unknown account")
	}
	f.reader.CacheOnly = true
	if _, err := f.reader.Credits(ctx, "cave"); err == nil {
		t.Error("credits in cache-only mode")
	}
	if f.agy.count() != 1 {
		t.Errorf("%d agy runs, want 1", f.agy.count())
	}
}
