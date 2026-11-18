package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The golden files come from the old Python renderers on the same synthetic
// input; testdata/gen/golden.py regenerates them.

const goldenNow = 1700000000

type goldenCase struct {
	columns  int
	color    bool
	absolute string
}

var goldenCases = []goldenCase{
	{80, false, "auto"}, {80, true, "auto"}, {120, false, "auto"}, {120, true, "auto"},
	{100, false, "y"}, {80, false, "y"}, {120, false, "n"}, {40, false, "auto"},
}

func (c goldenCase) tag() string {
	mode := "plain"
	if c.color {
		mode = "color"
	}
	return fmt.Sprintf("%d-%s-%s", c.columns, mode, c.absolute)
}

func (c goldenCase) opts() Options {
	return Options{Columns: c.columns, Color: c.color, Absolute: c.absolute,
		Now: time.Unix(goldenNow, 0), Location: time.UTC}
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func compare(t *testing.T, name string, got []byte) {
	t.Helper()
	want := readFile(t, name)
	if !bytes.Equal(got, want) {
		gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("%s: line %d differs\n got: %q\nwant: %q", name, i+1, g, w)
			}
		}
		t.Fatalf("%s differs", name)
	}
}

func goldenEnvelopes(t *testing.T) []Envelope {
	envs := ParseEnvelopes(readFile(t, "envelopes.jsonl"))
	for i := range envs {
		for k, v := range envs[i].CacheFiles {
			envs[i].CacheFiles[k] = filepath.Join("testdata", v)
		}
	}
	return envs
}

func TestGoldenAgy(t *testing.T) {
	var rows []json.RawMessage
	if err := json.Unmarshal(readFile(t, "agy-rows.json"), &rows); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"m": filepath.Join("testdata", "agy-cache.json")}
	for _, c := range goldenCases {
		t.Run(c.tag(), func(t *testing.T) {
			var b bytes.Buffer
			if err := RenderAgy(&b, rows, files, c.opts()); err != nil {
				t.Fatal(err)
			}
			compare(t, "agy-"+c.tag()+".txt", b.Bytes())
		})
	}
}

func TestGoldenAgentStatus(t *testing.T) {
	envs := goldenEnvelopes(t)
	for _, c := range goldenCases {
		t.Run(c.tag(), func(t *testing.T) {
			var b bytes.Buffer
			if err := RenderAgentStatus(&b, envs, c.opts()); err != nil {
				t.Fatal(err)
			}
			compare(t, "agent-status-"+c.tag()+".txt", b.Bytes())
		})
	}
}

func TestGoldenAgentStatusJSON(t *testing.T) {
	envs := goldenEnvelopes(t)
	got, err := agentStatusJSON(envs, time.Unix(goldenNow, 250_000_000), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	compare(t, "agent-status.json", got)
}
