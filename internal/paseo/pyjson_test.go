package paseo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The golden files come from testdata/gen_golden.py, which runs the old
// Python tool on synthetic inputs.

type digestCase struct {
	JSON       string `json:"json"`
	Dumps      string `json:"dumps"`
	Indent     string `json:"indent"`
	Digest     string `json:"digest"`
	FilePrefix string `json:"file_prefix"`
}

func readGolden(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func TestDigestsMatchPython(t *testing.T) {
	var cases []digestCase
	readGolden(t, "digests.json", &cases)
	if len(cases) < 8 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		v, err := Decode([]byte(c.JSON))
		if err != nil {
			t.Fatalf("%s: %v", c.JSON, err)
		}
		if got, _ := Dumps(v, true); got != c.Dumps {
			t.Errorf("dumps(%s)\n got %s\nwant %s", c.JSON, got, c.Dumps)
		}
		if got, _ := DumpIndent(v, 2); got != c.Indent {
			t.Errorf("indent(%s)\n got %s\nwant %s", c.JSON, got, c.Indent)
		}
		if got, _ := Digest(v); got != c.Digest {
			t.Errorf("digest(%s) = %s, want %s", c.JSON, got, c.Digest)
		}
		if c.FilePrefix != "" {
			if got := mustDigest(v)[:16]; got != c.FilePrefix {
				t.Errorf("file prefix of %s = %s, want %s", c.JSON, got, c.FilePrefix)
			}
		}
	}
}

func TestFloatRepr(t *testing.T) {
	for in, want := range map[float64]string{
		1: "1.0", 0.1: "0.1", 1e16: "1e+16", 1e15: "1000000000000000.0", 1e-5: "1e-05",
		0.0001: "0.0001", 1.5e300: "1.5e+300", -2.5: "-2.5", 123456789.125: "123456789.125",
	} {
		if got := FloatRepr(in); got != want {
			t.Errorf("FloatRepr(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestObjectKeepsOrderAndUpdateSemantics(t *testing.T) {
	o, err := DecodeObject([]byte(`{"b": 1, "a": 2, "b": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	o.Set("c", 4)
	o.Set("b", 5)
	got, _ := Dumps(o, false)
	if want := `{"b": 5, "a": 2, "c": 4}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, err := Decode([]byte(`{} x`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := DecodeObject([]byte(`[1]`)); err == nil {
		t.Fatal("non-object accepted")
	}
}

func TestPyHelpers(t *testing.T) {
	if pyStr(nil) != "None" || pyStr(json.Number("300")) != "300" || pyStr(json.Number("1E2")) != "100.0" || pyStr(true) != "True" {
		t.Fatal("pyStr")
	}
	for _, v := range []any{nil, false, "", json.Number("0"), json.Number("0.0"), []any{}, NewObject()} {
		if truthy(v) {
			t.Errorf("truthy(%#v)", v)
		}
	}
	for _, v := range []any{true, "x", json.Number("2"), []any{1}, Obj("a", 1)} {
		if !truthy(v) {
			t.Errorf("!truthy(%#v)", v)
		}
	}
}

func TestWithSuffixAndPathName(t *testing.T) {
	if got := withSuffix("/d/abc-x.json", ".lock"); got != "/d/abc-x.lock" {
		t.Fatal(got)
	}
	if got := withSuffix("/d/state", ".lock"); got != "/d/state.lock" {
		t.Fatal(got)
	}
	if pathName("/x/bin/agy/") != "agy" || pathName("agy") != "agy" {
		t.Fatal("pathName")
	}
}

func TestRealpath(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(real, "target"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "target"), filepath.Join(real, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(real, "loop")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		filepath.Join(dir, "link"):                  filepath.Join(real, "target"),
		filepath.Join(dir, "link", "missing", "x"):  filepath.Join(real, "target", "missing", "x"),
		filepath.Join(dir, "link", "..", "link"):    filepath.Join(real, "target"),
		filepath.Join(dir, "missing", "..", "link"): filepath.Join(real, "target"),
		filepath.Join(dir, "loop", "x"):             filepath.Join(real, "loop", "x"),
		"/":                                         "/",
		"//a/./b/":                                  "/a/b",
	}
	for in, want := range cases {
		if got := realpath(in); got != want {
			t.Errorf("realpath(%s) = %s, want %s", in, got, want)
		}
	}
}
