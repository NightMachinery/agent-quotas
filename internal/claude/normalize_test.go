package claude

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

func snapFor(t *testing.T, payload string, now float64) model.Snapshot {
	t.Helper()
	p, err := parseValue([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	r := &Reader{Location: time.UTC}
	return r.normalize("work", &tokenInfo{source: "env:X"}, &fetchResult{payload: p, fetchedAt: now, source: "api"}, nil, now)
}

const scopedPayload = `{"limits":[
 {"kind":"session","group":"session","percent":30,"resets_at":1800003600},
 {"kind":"weekly_all","group":"weekly","percent":50,"resets_at":1800300000},
 {"kind":"weekly_scoped","group":"weekly","percent":100,"resets_at":1800400000,"scope":{"model":{"display_name":"Sonnet"}}}]}`

func TestVerdictModelScopedDoesNotBlockAccount(t *testing.T) {
	s := snapFor(t, scopedPayload, testNow)
	if s.Verdict != model.Usable || s.BlockedUntil != nil {
		t.Fatalf("verdict %s", s.Verdict)
	}
	w := s.Windows[2]
	if w.Family != "Sonnet" || w.Scope != model.ScopeModel || !w.Blocking || w.Window != "weekly" {
		t.Errorf("scoped window %+v", w)
	}
	found := false
	for _, n := range s.Notes {
		found = found || n.Text == "7d Sonnet at 100%: blocks Sonnet only"
	}
	if !found {
		t.Errorf("notes %+v", s.Notes)
	}

	for fam, want := range map[string]bool{"sonnet": false, "opus": true, "": true} {
		got, err := UsableFor(s, fam)
		if err != nil || got != want {
			t.Errorf("UsableFor(%q) = %v, %v", fam, got, err)
		}
	}
}

func TestVerdictBlockedUntilLatestReset(t *testing.T) {
	s := snapFor(t, `{"limits":[
 {"kind":"session","percent":100,"resets_at":1800003600},
 {"kind":"weekly_all","percent":100.0,"resets_at":1800300000}]}`, testNow)
	if s.Verdict != model.Blocked || s.BlockedUntil == nil || *s.BlockedUntil != 1800300000 {
		t.Fatalf("verdict %s until %v", s.Verdict, s.BlockedUntil)
	}
	if strings.Join(s.BlockedReasons, ";") != "5h at 100%;weekly at 100%" {
		t.Errorf("reasons %v", s.BlockedReasons)
	}
	if ok, err := UsableFor(s, "opus"); ok || err != nil {
		t.Errorf("UsableFor: %v %v", ok, err)
	}
}

func TestVerdictUnknown(t *testing.T) {
	for name, payload := range map[string]string{
		"no windows":     `{}`,
		"only scoped":    `{"seven_day_opus":{"utilization":5,"resets_at":null}}`,
		"missing number": `{"limits":[{"kind":"session","percent":null},{"kind":"weekly_all","percent":3}]}`,
	} {
		if s := snapFor(t, payload, testNow); s.Verdict != model.Unknown {
			t.Errorf("%s: %s", name, s.Verdict)
		}
	}
	// A blocking window still wins over an unknown one.
	if s := snapFor(t, `{"limits":[{"kind":"session","percent":null},{"kind":"weekly_all","percent":100}]}`, testNow); s.Verdict != model.Blocked {
		t.Errorf("blocked+unknown: %s", s.Verdict)
	}
}

func TestLegacyWindowsAreNormalized(t *testing.T) {
	s := snapFor(t, `{"five_hour":{"utilization":0.25,"resets_at":"2027-01-15T10:00:00Z"},
 "seven_day":{"utilization":99,"resets_at":1800300000},"seven_day_opus":{"utilization":100,"resets_at":1800300000}}`, testNow)
	if s.Verdict != model.Usable || *s.Windows[0].UsedPercent != 25 || s.Windows[0].Window != "5h" || s.Windows[2].Family != "Opus" {
		t.Fatalf("%s %+v", s.Verdict, s.Windows)
	}
	if ok, _ := UsableFor(s, "opus"); ok {
		t.Error("opus should be blocked by seven_day_opus")
	}
	if ok, _ := UsableFor(s, "sonnet"); !ok {
		t.Error("sonnet should be usable")
	}
}

func TestUsableForErrors(t *testing.T) {
	if _, err := UsableFor(model.Snapshot{Verdict: model.SignedOut}, ""); !errors.Is(err, ErrQuotaUnavailable) {
		t.Errorf("signed out: %v", err)
	}
	if _, err := UsableFor(model.Snapshot{Error: model.S("boom")}, ""); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error snapshot: %v", err)
	}
	s := snapFor(t, `{"limits":[{"kind":"session","percent":null}]}`, testNow)
	if _, err := UsableFor(s, ""); err == nil || !strings.Contains(err.Error(), "utilization unavailable") {
		t.Errorf("nil pct: %v", err)
	}
	s = snapFor(t, `{"seven_day_opus":{"utilization":5}}`, testNow)
	if _, err := UsableFor(s, "sonnet"); err == nil || !strings.Contains(err.Error(), "no applicable") {
		t.Errorf("no window: %v", err)
	}
}

func TestModelFamily(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-4-1": "opus", "claude-sonnet-4-5-20250929": "sonnet", "opus": "opus",
		"Sonnet[1m]": "sonnet[1m]", "": "", "claude-": "", "haiku-4": "haiku",
	} {
		if got := ModelFamily(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestNotes(t *testing.T) {
	p, _ := parseValue([]byte(`{"five_hour":{"utilization":10,"resets_at":1800003600},
 "extra_usage":{"is_enabled":true,"spend_limit_reached":true,"used_credits":null,"monthly_limit":10},
 "iguana_necktie":{"limit_dollars":250,"used_dollars":2,"remaining_dollars":248,"resets_at":1800086400,"locked_reason":null}}`))
	r := &Reader{Location: time.UTC}
	tok := &tokenInfo{source: "keychain", expiresAt: model.F(testNow - 10), warning: "w"}
	s := r.normalize("work", tok, &fetchResult{payload: p, fetchedAt: testNow, source: "api"}, []string{"a warning"}, testNow)
	var texts []string
	for _, n := range s.Notes {
		texts = append(texts, n.Severity+":"+n.Text)
	}
	want := []string{
		"warning:a warning",
		"warning:OAuth token expired; " + loginHint,
		"warning:Extra usage: is_enabled: True, monthly_limit: 10, spend_limit_reached: True",
		"info:Cloud credit: $248.00 of $250.00 left, expires 2027-01-16 08:00:00 +0000",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Errorf("notes:\n%s", strings.Join(texts, "\n"))
	}
}

// Ported from test_claude_code_usage.py.

func creditPayload(t *testing.T, overrides string) *value {
	t.Helper()
	entry := map[string]any{"utilization": 0.8, "resets_at": testNow + 86400, "limit_dollars": 250,
		"used_dollars": 2, "remaining_dollars": 248, "locked_reason": nil}
	if overrides != "" {
		var o map[string]any
		json.Unmarshal([]byte(overrides), &o)
		for k, v := range o {
			entry[k] = v
		}
	}
	data, _ := json.Marshal(map[string]any{
		"five_hour": map[string]any{"limit_dollars": nil, "used_dollars": nil, "remaining_dollars": nil,
			"locked_reason": nil, "utilization": 0.0, "resets_at": testNow + 3600},
		"iguana_necktie": entry,
	})
	p, err := parseValue(data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCloudCreditFoundByNameAndNormalized(t *testing.T) {
	c := extractCloudCredit(creditPayload(t, ""), testNow, time.UTC)
	if c.Key != "iguana_necktie" || c.RemainingDollars != 248 || c.LimitDollars != 250 || *c.ExpiresAt != testNow+86400 || !c.Available {
		t.Errorf("%+v", c)
	}
}

func TestCloudCreditFoundByShapeWhenRenamed(t *testing.T) {
	p := creditPayload(t, "")
	p.obj["new_name"] = p.obj["iguana_necktie"]
	p.keys = append(p.keys, "new_name")
	delete(p.obj, "iguana_necktie")
	p.keys = []string{"five_hour", "new_name"}
	if c := extractCloudCredit(p, testNow, time.UTC); c == nil || c.Key != "new_name" {
		t.Errorf("%+v", c)
	}
}

func TestCloudCreditAbsentWhenDollarsAreNull(t *testing.T) {
	if c := extractCloudCredit(creditPayload(t, `{"limit_dollars":null,"used_dollars":null,"remaining_dollars":null}`), testNow, time.UTC); c != nil {
		t.Errorf("%+v", c)
	}
	p, _ := parseValue([]byte(`{"five_hour":{"limit_dollars":null,"remaining_dollars":null}}`))
	if c := extractCloudCredit(p, testNow, time.UTC); c != nil {
		t.Errorf("%+v", c)
	}
}

func TestCloudCreditIsNotAUsageWindow(t *testing.T) {
	ws := extractWindows(creditPayload(t, ""), time.UTC)
	if len(ws) != 1 || ws[0].key != "five_hour" {
		t.Errorf("%+v", ws)
	}
}

func TestPythonFormatting(t *testing.T) {
	for f, want := range map[float64]string{1: "1.0", 12.5: "12.5", 1e16: "1e+16", 1.5e-5: "1.5e-05", 0.0001: "0.0001",
		123456789012345.0: "123456789012345.0", -2.5: "-2.5", 1800010000.123: "1800010000.123"} {
		if got := pyFloatRepr(f); got != want {
			t.Errorf("repr(%v) = %s, want %s", f, got, want)
		}
	}
	for f, want := range map[float64]string{1234.5: "1,234.50", 0: "0.00", 1234567.891: "1,234,567.89", 12: "12.00"} {
		if got := pyMoney(f); got != want {
			t.Errorf("money(%v) = %s", f, got)
		}
	}
	for f, want := range map[float64]string{91.25: "91.2", 0.05: "0.1", 2.675: "2.7", 99.96: "100"} {
		if got := pyG(pyRound1(f)); got != want {
			t.Errorf("round1(%v) = %s, want %s", f, got, want)
		}
	}
	v, _ := parseValue([]byte(`{"s":"it's","d":{"q":"a\"b","n":null,"l":[1,2.0,false]}}`))
	if got := v.pyRepr(); got != `{'s': "it's", 'd': {'q': 'a"b', 'n': None, 'l': [1, 2.0, False]}}` {
		t.Errorf("repr: %s", got)
	}
}

func TestParseISO(t *testing.T) {
	utc := time.UTC
	for in, want := range map[string]float64{
		"2027-01-15T08:00:00+00:00":           1800000000,
		"2027-01-15T08:00:00.123456789+00:00": 1800000000.123456,
		"2027-01-15 11:30:00+0330":            1800000000,
		"2027-01-15T08:00+00:00":              1800000000,
		"2027-01-15T08":                       1800000000,
		"2027-01-15T08:00:00.5":               1800000000.5,
		"2027-01-15T08:00:00-01:00":           1800003600,
	} {
		got := parseISO(in, utc)
		if got == nil || *got != want {
			t.Errorf("%s: %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"tomorrow", "2027-13-01", "2027-01-15X08:00", "2027-01-15T8:00"} {
		if got := parseISO(bad, utc); got != nil {
			t.Errorf("%s parsed as %v", bad, *got)
		}
	}
}
