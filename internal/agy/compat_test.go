package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/render"
)

// Ports of zshlang/tests/agy-status-buckets.zsh and the quota part of
// python/test_agy_model_catalog.py.

func TestBucketIDs(t *testing.T) {
	for _, c := range []struct{ group, label, want string }{
		{"Gemini Models", "Weekly Limit Remaining", "gemini-weekly"},
		{"Claude and GPT models", "5h Limit Remaining", "3p-5h"},
		{"CLAUDE AND GPT MODELS", "Weekly", "3p-weekly"},
		{"Gemini 4 Argon", "Gemini 4 Argon Weekly Limit", "gemini-4-argon-weekly"},
		{"Gemini 4 Argon", "Gemini 4 Argon 5 Hour Limit", "gemini-4-argon-5h"},
		{"Future Model", "Weekly Limit", "future-model-weekly"},
		{"Future Model", "Daily Limit", "future-model-daily-limit"},
		{"--Odd--", "", "odd"},
	} {
		if got := BucketID(c.group, c.label); got != c.want {
			t.Errorf("BucketID(%q, %q) = %q, want %q", c.group, c.label, got, c.want)
		}
	}
	for id, want := range map[string][2]string{
		"gemini-5h":             {"Gemini Models", "5h Limit Remaining"},
		"3p-weekly":             {"Claude and GPT models", "Weekly Limit Remaining"},
		"gemini-4-argon-weekly": {"Gemini 4 Argon", "Weekly Limit Remaining"},
		"3p-extra-5h":           {"3p Extra", "5h Limit Remaining"},
		"strange":               {"strange", "Limit Remaining"},
	} {
		if g, l := bucketDisplayNames(id); g != want[0] || l != want[1] {
			t.Errorf("names(%q) = %q, %q", id, g, l)
		}
	}
}

func TestAliasesDeduplicateByCapture(t *testing.T) {
	doc, err := parseRawObject([]byte(`{"buckets":{
	 "gemini-4-argon-weekly":{"remaining_percent":17,"captured_at":10},
	 "gemini-argon-wtu-weekly":{"remaining_percent":99,"captured_at":1},
	 "gemini-argon-5h":{"remaining_percent":30,"captured_at":5}}}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := cacheRows(doc, 100, -1)
	if len(rows) != 2 || rows[0].ID != "gemini-4-argon-weekly" || rows[0].Remaining != 17 || rows[1].ID != "gemini-4-argon-5h" {
		t.Errorf("rows %+v", rows)
	}
}

func buckets(pairs ...any) []json.RawMessage {
	var out []json.RawMessage
	for i := 0; i < len(pairs); i += 2 {
		b, _ := json.Marshal(map[string]any{"bucket_id": pairs[i], "remaining_percent": pairs[i+1]})
		out = append(out, b)
	}
	return out
}

func TestQuotaForModel(t *testing.T) {
	rows := buckets("gemini-weekly", 90, "gemini-5h", 80, "gemini-4-argon-weekly", 55, "gemini-4-argon-5h", 40,
		"gemini-3-8-flash-5h", 0, "3p-weekly", 0, "gemini-4-argon-low-5h", 0)
	remaining, ids, err := QuotaForModel(rows, "gemini-4-argon-high")
	if err != nil || remaining != 40 || strings.Join(ids, ",") != "gemini-weekly,gemini-5h,gemini-4-argon-weekly,gemini-4-argon-5h" {
		t.Errorf("argon: %v %v %v", remaining, ids, err)
	}
	if r, _, _ := QuotaForModel(rows, "gemini-3.8-flash-low"); r != 0 {
		t.Errorf("flash: %v", r)
	}
	if r, _, _ := QuotaForModel(buckets("gemini-weekly", 65, "gemini-4-weekly", 2, "gemini-4-argon-weekly", 70), "gemini-4-argon-high"); r != 2 {
		t.Errorf("nested: %v", r)
	}
	if r, _, _ := QuotaForModel(buckets("3p-weekly", 70, "claude-opus-5-5-5h", 30, "claude-sonnet-5-5-5h", 0), "claude-opus-5-5-low"); r != 30 {
		t.Errorf("3p: %v", r)
	}
	for _, bad := range []any{nil, -1, 101, true, "50"} {
		if _, _, err := QuotaForModel(buckets("gemini-weekly", 80, "gemini-4-argon-5h", bad), "gemini-4-argon-high"); err == nil {
			t.Errorf("value %v should fail closed", bad)
		}
	}
	if _, _, err := QuotaForModel(rows, "mystery-model"); err == nil {
		t.Error("unknown family")
	}
	if _, _, err := QuotaForModel(buckets("3p-weekly", 50), "gemini-4-argon-high"); err == nil {
		t.Error("no applicable pool")
	}
}

func TestUsableFor(t *testing.T) {
	snap := func(native string) model.Snapshot {
		return model.Snapshot{ID: "agy:m", Native: json.RawMessage(native)}
	}
	if ok, err := UsableFor(snap(`{"error":null,"signed_in":null,"buckets":[{"bucket_id":"gemini-weekly","remaining_percent":5}]}`), "gemini-4-argon-high"); !ok || err != nil {
		t.Errorf("usable: %v %v", ok, err)
	}
	if ok, err := UsableFor(snap(`{"buckets":[{"bucket_id":"gemini-weekly","remaining_percent":0}]}`), "gemini-4-argon-high"); ok || err != nil {
		t.Errorf("exhausted: %v %v", ok, err)
	}
	for _, native := range []string{``, `{"error":"Quota check failed","buckets":[]}`, `{"signed_in":false,"buckets":[]}`} {
		if _, err := UsableFor(snap(native), "gemini-4-argon-high"); err == nil {
			t.Errorf("%q should be an error", native)
		}
	}
}

// A model is blocked while any of its pools is empty, until the latest of
// their resets; the other family's pools do not count.
func TestModelSeat(t *testing.T) {
	snap := model.Snapshot{ID: "agy:m", Native: json.RawMessage(`{"buckets":[
		{"bucket_id":"gemini-weekly","remaining_percent":15,"resets_at":2000000900},
		{"bucket_id":"gemini-5h","remaining_percent":0,"resets_at":2000000100},
		{"bucket_id":"3p-5h","remaining_percent":0,"resets_at":2000000500}]}`)}
	usable, at, err := ModelSeat(snap, "gemini-4-neon-high")
	if err != nil || usable || at != 2000000100 {
		t.Fatalf("gemini: %v %v %v", usable, at, err)
	}
	snap.Native = json.RawMessage(`{"buckets":[{"bucket_id":"gemini-weekly","remaining_percent":0,"resets_at":2000000900},
		{"bucket_id":"gemini-5h","remaining_percent":0,"resets_at":2000000100}]}`)
	if _, at, _ := ModelSeat(snap, "gemini-4-neon-high"); at != 2000000900 {
		t.Fatalf("latest reset: %v", at)
	}
	snap.Native = json.RawMessage(`{"buckets":[{"bucket_id":"gemini-5h","remaining_percent":40}]}`)
	if usable, at, err := ModelSeat(snap, "gemini-4-neon-high"); !usable || at != 0 || err != nil {
		t.Fatalf("usable: %v %v %v", usable, at, err)
	}
	// Once its reset has passed, an empty pool in an old reading no longer
	// blocks: nothing refreshes the reading while the account's agents wait.
	snap.Native = json.RawMessage(`{"buckets":[{"bucket_id":"gemini-weekly","remaining_percent":15,"resets_at":2000000900},
		{"bucket_id":"gemini-5h","remaining_percent":0,"resets_at":2000000100}]}`)
	defer func(old func() time.Time) { now = old }(now)
	now = func() time.Time { return time.Unix(2000000101, 0) }
	if usable, _, err := ModelSeat(snap, "gemini-4-neon-high"); !usable || err != nil {
		t.Fatalf("after the reset: %v %v", usable, err)
	}
	if ok, _ := UsableFor(snap, "gemini-4-neon-high"); !ok {
		t.Fatal("UsableFor after the reset")
	}
}

// The human agy-status report renders the readers' Native rows, with the
// cache age read from each account's cache.
func TestHumanReportFromSnapshots(t *testing.T) {
	f := newFixture(t)
	var rows []json.RawMessage
	for _, name := range []string{"m", "fm2"} {
		snap, err := f.reader.Read(context.Background(), ref(name), at(1999990400))
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, snap.Native)
	}
	var b bytes.Buffer
	err := render.RenderAgy(&b, rows, f.reader.CacheFiles(), render.Options{Columns: 80, Now: time.Unix(1999990400, 0), Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	text := b.String()
	for _, want := range []string{"m  ", "cache 1m, some undated", "Gemini 4 Argon", "Claude/GPT", "agy-acc-login fm2"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
}
