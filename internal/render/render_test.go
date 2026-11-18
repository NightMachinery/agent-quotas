package render

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Ports of python/test_agy_status_render.py and
// python/test_agent_status_render.py.

const now0 = 1700000000

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func bucketRow(id, group string, used float64, seconds int64) map[string]any {
	return map[string]any{"bucket_id": id, "group": group, "label": "Weekly Limit Remaining",
		"used_percent": used, "remaining_percent": 100 - used, "resets_at": now0 + seconds, "source": "cache"}
}

func fixture() []map[string]any {
	return []map[string]any{
		{"account": "m", "label": "m", "signed_in": nil, "tier": "Google AI Pro",
			"email": "masked@example.test", "error": nil, "buckets": []any{
				bucketRow("gemini-weekly", "Gemini Models", 6, 4*86400+7*3600),
				bucketRow("3p-weekly", "Claude and GPT models", 75, 2*86400+11*3600),
				bucketRow("gemini-4-argon-weekly", "Gemini 4 Argon", 95, 12*60),
				bucketRow("gemini-5h", "Gemini Models", 6, 3600+25*60)}},
		{"account": "cave", "label": "研究 Café", "signed_in": true, "tier": "Pro",
			"email": "another@example.test", "error": nil, "buckets": []any{
				bucketRow("3p-5h", "Claude and GPT models", 12.5, 13*60)}},
		{"account": "fm2", "label": "fm2", "signed_in": false, "tier": nil, "error": nil, "buckets": []any{}},
		{"account": "broken", "label": "broken", "signed_in": nil, "tier": nil,
			"error": "Quota lookup failed", "buckets": []any{}},
	}
}

func renderRows(t *testing.T, rows []map[string]any, files map[string]string, columns int, color bool) string {
	t.Helper()
	var raws []json.RawMessage
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, b)
	}
	var out bytes.Buffer
	opts := Options{Columns: columns, Color: color, Now: time.Unix(now0, 0), Location: time.UTC}
	if err := RenderAgy(&out, raws, files, opts); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestDuration(t *testing.T) {
	for in, want := range map[float64]string{
		4*86400 + 7*3600 + 59*60: "4d 7h", 3600 + 25*60: "1h 25m", 12 * 60: "12m", 5: "<1m", -5: "<1m",
	} {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPastAndUnknownResets(t *testing.T) {
	rows := fixture()
	b := rows[0]["buckets"].([]any)
	b[0].(map[string]any)["resets_at"] = now0 - 3600
	b[1].(map[string]any)["resets_at"] = nil
	text := renderRows(t, rows, nil, 80, false)
	for _, want := range []string{"reset 1h ago", "reset unknown"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
}

func TestWidthsAlignmentAndRows(t *testing.T) {
	if w := Width("研究 Café"); w != 9 {
		t.Fatalf("Width = %d, want 9", w)
	}
	for _, columns := range []int{80, 120} {
		text := renderRows(t, fixture(), nil, columns, false)
		positions := map[int]bool{}
		for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			if Width(line) > columns {
				t.Errorf("line wider than %d: %q", columns, line)
			}
			if i := strings.IndexAny(line, "█░"); i >= 0 {
				positions[Width(line[:i])] = true
			}
		}
		if len(positions) != 1 {
			t.Errorf("bars not aligned at %d columns: %v", columns, positions)
		}
		for _, want := range []string{"Gemini 4 Argon", "Claude/GPT", "resets in 4d 7h", "resets in 1h 25m", "agy-acc-login fm2"} {
			if !strings.Contains(text, want) {
				t.Errorf("%d: missing %q", columns, want)
			}
		}
		if strings.Index(text, "5h") > strings.Index(text, "weekly") {
			t.Error("5h should come before weekly")
		}
		if strings.Contains(text, "Limit Remaining") || strings.Contains(text, "\x1b") {
			t.Error("chrome or colour leaked")
		}
		if strings.Count(text, "Quota lookup failed") != 1 {
			t.Error("error row should appear once")
		}
	}
}

func TestCacheAgeHeaderAndDimAbsolute(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache.json")
	rows := fixture()
	buckets := map[string]any{}
	for _, b := range rows[0]["buckets"].([]any) {
		buckets[b.(map[string]any)["bucket_id"].(string)] = map[string]any{"captured_at": now0 - 120}
	}
	data, _ := json.Marshal(map[string]any{"buckets": buckets})
	if err := os.WriteFile(cache, data, 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"m": cache}
	text := renderRows(t, rows, files, 120, true)
	first := strings.SplitN(text, "\n", 2)[0]
	for _, want := range []string{"\x1b[2mcache 2m\x1b[0m", "\x1b[2mmasked@example.test"} {
		if !strings.Contains(first, want) {
			t.Errorf("header %q lacks %q", first, want)
		}
	}
	for _, code := range []string{"\x1b[32m", "\x1b[33m", "\x1b[31m"} {
		if !strings.Contains(text, code) {
			t.Errorf("missing colour %q", code)
		}
	}
	stamp := regexp.MustCompile("\x1b\\[2m\\w{3} \\d{2} \\w{3}")
	if !stamp.MatchString(text) {
		t.Error("absolute stamp missing at 120 columns")
	}
	if stamp.MatchString(renderRows(t, rows, files, 80, true)) {
		t.Error("absolute stamp shown at 80 columns")
	}
}

func TestHeaderColumnsAlign(t *testing.T) {
	rows := fixture()[:3]
	for i, v := range []struct{ label, tier, source string }{{"m", "Pro", "cache"}, {"cave", "Google AI Pro", "live"}, {"研究", "Ultra", "cache"}} {
		rows[i]["label"], rows[i]["signed_in"], rows[i]["tier"], rows[i]["email"] = v.label, nil, v.tier, "masked@example.test"
		for _, b := range rows[i]["buckets"].([]any) {
			b.(map[string]any)["source"] = v.source
		}
	}
	for _, columns := range []int{80, 120} {
		text := renderRows(t, rows, nil, columns, true)
		var headers []string
		for _, block := range strings.Split(text, "\n\n") {
			headers = append(headers, ansi.ReplaceAllString(strings.SplitN(block, "\n", 2)[0], ""))
		}
		for _, tokens := range [][]string{{"auth stored", "auth stored", "auth stored"}, {"Pro", "Google AI Pro", "Ultra"}, {"masked", "masked", "masked"}} {
			seen := map[int]bool{}
			for i, h := range headers {
				seen[Width(h[:strings.Index(h, tokens[i])])] = true
			}
			if len(seen) != 1 {
				t.Errorf("%d columns: %v not aligned: %q", columns, tokens, headers)
			}
		}
	}
}

func TestColorThresholds(t *testing.T) {
	for _, c := range []struct {
		used float64
		code string
	}{{69.9, "\x1b[32m"}, {70, "\x1b[33m"}, {89.9, "\x1b[33m"}, {90, "\x1b[31m"}} {
		rows := fixture()[:1]
		b := rows[0]["buckets"].([]any)[:1]
		b[0].(map[string]any)["used_percent"] = c.used
		rows[0]["buckets"] = b
		if text := renderRows(t, rows, nil, 80, true); !strings.Contains(text, c.code) {
			t.Errorf("used %v: missing %q", c.used, c.code)
		}
	}
}

func TestLongHeaderDoesNotWrap(t *testing.T) {
	rows := fixture()
	rows[0]["label"] = strings.Repeat("A very long ", 20)
	rows[0]["tier"] = strings.Repeat("Plan ", 20)
	for _, b := range rows[0]["buckets"].([]any) {
		b.(map[string]any)["source"] = "live"
	}
	text := renderRows(t, rows, nil, 80, false)
	if !strings.Contains(strings.SplitN(text, "\n", 2)[0], "live") {
		t.Error("live provenance missing")
	}
	for _, line := range strings.Split(text, "\n") {
		if Width(line) > 80 {
			t.Errorf("wrapped: %q", line)
		}
	}
}

func TestColorRules(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if AgyColor("always", true) || AgentStatusColor("always", true) {
		t.Error("NO_COLOR must win")
	}
	os.Unsetenv("NO_COLOR")
	if AgyColor("always", false) {
		t.Error("agy-status never colours a pipe")
	}
	if !AgentStatusColor("always", false) || AgentStatusColor("auto", false) || !AgyColor("auto", true) {
		t.Error("colour modes")
	}
}

func claudeEnvelope() Envelope {
	return Envelope{Agent: "claude", Exit: 1, Report: json.RawMessage(`[
	 {"profile": "default", "plan": "max", "cache": {"from_cache": true, "fetched_at": 1699999880},
	  "windows": [{"key": "session", "label": "5h session", "utilization_percent": 52, "resets_at": 1700005100},
	              {"key": "weekly_scoped", "label": "7d Fable", "utilization_percent": 90, "resets_at": 1700370800}]},
	 {"profile": "work", "error": "Login required"}]`)}
}

func section(env Envelope, columns int, color bool) string {
	return renderSection(env, columns, color, "auto", now0, time.UTC)
}

func TestClaudeSection(t *testing.T) {
	for _, columns := range []int{80, 120} {
		text := section(claudeEnvelope(), columns, true)
		for _, line := range strings.Split(ansi.ReplaceAllString(text, ""), "\n") {
			if Width(line) > columns {
				t.Errorf("too wide: %q", line)
			}
		}
		if !strings.Contains(text, "█") || !strings.Contains(text, "\x1b[31m") {
			t.Error("bars or alarm colour missing")
		}
	}
	text := section(claudeEnvelope(), 80, false)
	for _, want := range []string{"cache 2m", "resets in 1h 25m", "resets in 4d 7h", "Fable", "Login required"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestCloudCreditOnlyWhileMoneyIsLeft(t *testing.T) {
	with := func(credit string) string {
		env := claudeEnvelope()
		env.Report = json.RawMessage(strings.Replace(string(env.Report), `"plan": "max",`, `"plan": "max", "cloud_credit": `+credit+`,`, 1))
		return section(env, 80, false)
	}
	credit := `{"limit_dollars": 250.0, "remaining_dollars": 248.03, "expires_at": 1700093600, "locked_reason": null, "available": %s}`
	if text := with(strings.Replace(credit, "%s", "true", 1)); !strings.Contains(text, "Cloud credit: $248.03 of $250.00 left, expires in 1d 2h") {
		t.Errorf("credit note missing:\n%s", text)
	}
	for _, hidden := range []string{
		strings.Replace(strings.Replace(credit, "%s", "false", 1), "248.03", "0.0", 1),
		strings.Replace(credit, "%s", "false", 1), "null"} {
		if strings.Contains(with(hidden), "Cloud credit") {
			t.Errorf("credit shown for %s", hidden)
		}
	}
}

func TestMissingUsageIsUnknownNotZero(t *testing.T) {
	env := claudeEnvelope()
	env.Report = json.RawMessage(strings.Replace(string(env.Report), `"utilization_percent": 52`, `"utilization_percent": null`, 1))
	text := section(env, 80, false)
	if !strings.Contains(text, "Claude 5h: usage unavailable") || regexp.MustCompile(`\b0%`).MatchString(text) || !strings.Contains(text, "Fable") {
		t.Errorf("unexpected:\n%s", text)
	}
}

func TestSectionErrorsStayInTheirSection(t *testing.T) {
	stream := []byte(`{"agent":"claude","exit":0,"report":[{"profile":"default"}]}` + "\ninvalid\n" +
		`{"agent":"codex","exit":3,"report":null}` + "\n")
	var out bytes.Buffer
	if err := RenderAgentStatus(&out, ParseEnvelopes(stream), Options{Columns: 80, Now: time.Unix(now0, 0)}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	a, b, c := strings.Index(text, "Claude Code"), strings.Index(text, "Unknown provider"), strings.Index(text, "Status lookup failed (exit 3)")
	if !(a >= 0 && a < b && b < c) || strings.Contains(text, "\x1b") {
		t.Errorf("order or colour wrong:\n%s", text)
	}
}

func TestJSONIsTheSameAccountsAsData(t *testing.T) {
	stream := []byte(`{"agent":"claude","exit":1,"report":` + string(claudeEnvelope().Report) + "}\ninvalid\n")
	doc, err := AgentStatusJSON(ParseEnvelopes(bytes.ReplaceAll(stream, []byte("\n\t"), []byte(" "))), time.Unix(now0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		GeneratedAt float64 `json:"generated_at"`
		Providers   []struct {
			Agent, Title string
			Error        *string
			Accounts     []struct {
				Tier, Source string
				Error        *string
				Windows      []struct {
					Family, Window string
					UsedPercent    float64 `json:"used_percent"`
					ResetsAt       *float64
				}
			}
		}
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Providers) != 2 || parsed.Providers[0].Title != "Claude Code" || parsed.Providers[0].Error != nil ||
		parsed.Providers[1].Error == nil || *parsed.Providers[1].Error != "Unknown provider" {
		t.Fatalf("providers: %s", doc)
	}
	def, work := parsed.Providers[0].Accounts[0], parsed.Providers[0].Accounts[1]
	if def.Tier != "max" || !strings.HasPrefix(def.Source, "cache ") || work.Error == nil || *work.Error != "Login required" {
		t.Errorf("accounts: %s", doc)
	}
	if w := def.Windows; len(w) != 2 || w[0].Family != "Claude" || w[0].UsedPercent != 52 || w[1].Family != "Fable" {
		t.Errorf("windows: %+v", w)
	}
	if bytes.Contains(doc, []byte("_display")) {
		t.Error("display fields leaked")
	}
}

func TestPythonNumbers(t *testing.T) {
	for f, want := range map[float64]string{52: "52.0", 1e16: "1e+16", 1.5e16: "1.5e+16", 0.0001: "0.0001", 1e-5: "1e-05", 1700000000.25: "1700000000.25"} {
		if got := pyFloatRepr(f); got != want {
			t.Errorf("repr(%v) = %q, want %q", f, got, want)
		}
	}
	for f, want := range map[float64]string{61: "61", 61.5: "61.5", 44.125: "44.125", 1234567: "1.23457e+06", 0.00001: "1e-05", 100: "100"} {
		if got := fmtG(Float(f)); got != want {
			t.Errorf("%v:g = %q, want %q", f, got, want)
		}
	}
	if got := fmtMoney(1234567.891); got != "1,234,567.89" {
		t.Errorf("money %q", got)
	}
	if got := pyTitle("gemini 4 argon 3p"); got != "Gemini 4 Argon 3P" {
		t.Errorf("title %q", got)
	}
}
