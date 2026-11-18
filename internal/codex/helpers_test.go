package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPyFormatting(t *testing.T) {
	reprs := map[float64]string{
		52: "52.0", 0.1: "0.1", 99.83333333333333: "99.83333333333333", 1e16: "1e+16",
		1e15: "1000000000000000.0", 1.5e-05: "1.5e-05", 0.0001: "0.0001", -2.5: "-2.5", 1790000000.5: "1790000000.5",
	}
	for f, want := range reprs {
		if got := pyFloatRepr(f); got != want {
			t.Errorf("repr(%v) = %q, want %q", f, got, want)
		}
	}
	gs := map[float64]string{
		52: "52", 31.5: "31.5", 33.3333: "33.3333", 64.25: "64.25", 1234567: "1.23457e+06",
		0.00001234: "1.234e-05", 100: "100", 10080: "10080", 2.5: "2.5", 99.83333333: "99.8333",
	}
	for f, want := range gs {
		if got := pyG(f); got != want {
			t.Errorf("format(%v, 'g') = %q, want %q", f, got, want)
		}
	}
	if got := pyRound1(57.55); got != 57.5 { // 57.55 is below the tie in binary
		t.Errorf("round(57.55, 1) = %v", got)
	}
	// Neumaier: plain left-to-right addition would give 0.
	if got := pySum([]float64{1e100, 1.0, -1e100}); got != 1.0 {
		t.Errorf("sum = %v, want 1.0", got)
	}
	if got := dumpCompact(mustParse(t, []byte(`{"a":[1,2.0,"<é>\u0001"],"b":{}}`))); got != `{"a": [1, 2.0, "<é>\u0001"], "b": {}}` {
		t.Errorf("dumpCompact = %s", got)
	}
}

func TestWindowLabelsAndBlockState(t *testing.T) {
	labels := map[float64]string{300: "5h", 10080: "Weekly", 1440: "Daily", 43200: "Monthly", 30: "30m", 90: "1.5h", 2880: "2d", 1500: "1500m", 0: "?"}
	for mins, want := range labels {
		m := mins
		if got := formatWindowDuration(&m); got != want {
			t.Errorf("label(%v) = %q, want %q", mins, got, want)
		}
	}
	// Prolite: a weekly primary and a null secondary is one window, not an
	// exhausted secondary.
	lim := mustParse(t, []byte(`{"primary":{"usedPercent":40,"windowDurationMins":10080,"resetsAt":200},"secondary":null,"credits":{"usedPercent":100}}`))
	if ws := iterWindows(lim); len(ws) != 1 || ws[0].Label != "Weekly" {
		t.Fatalf("windows = %+v", ws)
	}
	if st := computeBlockState(lim, nil, 100); st.Blocked {
		t.Errorf("prolite at 40%% must be usable: %+v", st)
	}
	// Signal only: the deadline falls back to the furthest reset reported.
	lim = mustParse(t, []byte(`{"primary":{"usedPercent":10,"resetsAt":100},"secondary":{"usedPercent":20,"resetsAt":300}}`))
	st := computeBlockState(lim, mustParse(t, []byte(`{"spendControlReached":true}`)), 100)
	if !st.Blocked || *st.ResetsAt != 300 || st.Reasons[0] != "spend control reached" {
		t.Errorf("signal block = %+v", st)
	}
	// The latest reset among blocked windows wins.
	lim = mustParse(t, []byte(`{"primary":{"usedPercent":100,"resetsAt":100},"secondary":{"usedPercent":100,"resetsAt":900},"x":{"usedPercent":5,"resetsAt":5000}}`))
	if st := computeBlockState(lim, nil, 100); *st.ResetsAt != 900 {
		t.Errorf("resetsAt = %v", *st.ResetsAt)
	}
	// No windows at all is unconstrained.
	if st := computeBlockState(mustParse(t, []byte(`{}`)), nil, 100); st.Blocked {
		t.Errorf("empty limit blocked")
	}
}

// TestExecLauncher drives the real pipe implementation with a shell script
// standing in for codex.
func TestExecLauncher(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	body := `#!/bin/sh
read -r line; echo '{"method":"note"}'; echo '{"id":"1","result":{}}'
read -r line
read -r line; echo '{"id":"2","result":{"account":{"email":"masked@example.test"}}}'
read -r line; echo "$CODEX_HOME" >&2; echo '{"id":"3","result":{"rateLimits":{"primary":{"usedPercent":7}}}}'
trap 'exit 0' TERM
sleep 30 & wait
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + dir + ":/usr/bin:/bin", "CODEX_HOME=/fake/home"}
	start := time.Now()
	res, err := runSession(context.Background(), ExecLauncher{}, []string{"codex", "app-server"}, env, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := dumpTight(res.Rate); string(got) != `{"rateLimits":{"primary":{"usedPercent":7}}}` {
		t.Errorf("rate = %s", got)
	}
	if asObj(res.Account.V("account")).V("email") != "masked@example.test" {
		t.Errorf("account = %s", dumpTight(res.Account))
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("session took %v", d)
	}

	// A missing binary is reported as such.
	_, err = runSession(context.Background(), ExecLauncher{}, []string{"codex-missing-xyz"}, env, time.Second)
	if err == nil || !strings.Contains(err.Error(), "codex not found in PATH") {
		t.Errorf("err = %v", err)
	}
}
