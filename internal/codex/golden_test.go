package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// goldenNow is the fixed clock of testdata/gen_goldens.py.
var goldenNow = time.Unix(1790000000, 0)

type goldenCase struct {
	Files     map[string]string `json:"files"`
	AuthFiles []json.RawMessage `json:"authFiles"`
}

// fakeHome writes the case's auth files under a temporary HOME.
func fakeHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(home, strings.TrimPrefix(rel, "~/"))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func diffText(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			t.Errorf("%s: first difference at line %d:\n got: %q\nwant: %q", name, i+1, gl, wl)
			return
		}
	}
	t.Errorf("%s: outputs differ", name)
}

func TestGoldens(t *testing.T) {
	inputs, err := filepath.Glob("testdata/cases/*.input.json")
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no golden inputs: %v", err)
	}
	sort.Strings(inputs)
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".input.json")
		data, err := os.ReadFile(in)
		if err != nil {
			t.Fatal(err)
		}
		var c goldenCase
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		goldens, _ := filepath.Glob("testdata/cases/" + name + ".*.golden")
		if len(goldens) == 0 {
			t.Fatalf("%s: no goldens", name)
		}
		home := fakeHome(t, c.Files)
		for _, g := range goldens {
			suffix := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(g), name+"."), ".golden")
			want, _ := os.ReadFile(g)
			t.Run(name+"/"+suffix, func(t *testing.T) {
				parts := strings.Split(suffix, ".")
				all := parts[0] == "all"
				fullPct := DefaultFullPct
				opts := RenderOptions{All: all, Now: goldenNow, Location: time.UTC, Home: home,
					ActivePath: filepath.Join(home, ".codex", "auth.json")}
				for _, p := range parts[1 : len(parts)-1] {
					switch p {
					case "fp50":
						fullPct = 50
					case "ansi":
						opts.Color = true
					case "neon":
						opts.Color, opts.TrueColor = true, true
					}
				}
				opts.FullPct = fullPct
				var got []byte
				switch parts[len(parts)-1] {
				case "json":
					got, err = LegacyDocument(c.AuthFiles, all, goldenNow, fullPct)
					if err != nil {
						t.Fatal(err)
					}
				case "txt":
					var b bytes.Buffer
					if err := RenderHuman(&b, c.AuthFiles, opts); err != nil {
						t.Fatal(err)
					}
					got = b.Bytes()
				}
				diffText(t, suffix, got, want)
			})
		}
	}
}
