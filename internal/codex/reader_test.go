package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
)

// fakeConn answers JSON-RPC requests with a handler.
type fakeConn struct {
	mu      sync.Mutex
	lines   chan []byte
	handle  func(method string, req *obj) []string
	methods []string
	stopped bool
	stderr  string
}

func (c *fakeConn) Send(line []byte) error {
	v, err := parseJSON(line)
	if err != nil {
		return err
	}
	req := v.(*obj)
	method, _ := req.V("method").(string)
	c.mu.Lock()
	c.methods = append(c.methods, method)
	c.mu.Unlock()
	for _, out := range c.handle(method, req) {
		c.lines <- []byte(out + "\n")
	}
	return nil
}

func (c *fakeConn) Lines() <-chan []byte { return c.lines }

func (c *fakeConn) Stop() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	return c.stderr
}

// fakeLauncher records each launch and lets the test act inside the
// isolated CODEX_HOME.
type fakeLauncher struct {
	mu       sync.Mutex
	launches []launch
	err      error
	// respond builds the handler for one launch, given its CODEX_HOME.
	respond func(codexHome string) (func(method string, req *obj) []string, string)
	conns   []*fakeConn
}

type launch struct {
	argv      []string
	env       []string
	codexHome string
	authBytes []byte
	files     []string
}

func envGet(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

func (l *fakeLauncher) Launch(ctx context.Context, argv, env []string) (Conn, error) {
	if l.err != nil {
		return nil, l.err
	}
	home := envGet(env, "CODEX_HOME")
	auth, _ := os.ReadFile(filepath.Join(home, "auth.json"))
	entries, _ := os.ReadDir(home)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	handle, stderr := l.respond(home)
	c := &fakeConn{lines: make(chan []byte, 16), handle: handle, stderr: stderr}
	l.mu.Lock()
	l.launches = append(l.launches, launch{argv: argv, env: env, codexHome: home, authBytes: auth, files: names})
	l.conns = append(l.conns, c)
	l.mu.Unlock()
	return c, nil
}

func reply(id, result string) string { return `{"id":"` + id + `","result":` + result + `}` }

// okHandler answers like a healthy app-server with the given rate result.
func okHandler(rate string) func(string, *obj) []string {
	return func(method string, req *obj) []string {
		switch method {
		case "initialize":
			return []string{`{"method":"noise"}`, `not json`, reply("1", `{"codexHome":"/x"}`)}
		case "account/read":
			return []string{reply("2", `{"account":{"type":"chatgpt","email":"masked@example.test","planType":"plus"}}`)}
		case "account/rateLimits/read":
			return []string{reply("3", rate)}
		}
		return nil
	}
}

const rateTwoWindows = `{"rateLimits":{"limitId":"codex","primary":{"usedPercent":52,"windowDurationMins":300,"resetsAt":1790008400},"secondary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":1790277200},"planType":"plus"},` +
	`"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":52,"windowDurationMins":300,"resetsAt":1790008400},"secondary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":1790277200}},` +
	`"codex_bengalfox":{"limitId":"codex_bengalfox","limitName":"Spark","primary":{"usedPercent":5,"windowDurationMins":300,"resetsAt":1790003600}},` +
	`"base_model_inference":{"limitId":"base_model_inference","limitName":"gpt-reserve","primary":{"usedPercent":1,"windowDurationMins":10080,"resetsAt":1790500000}}}}`

func jwt(t *testing.T, claims string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(claims)) + ".sig"
}

func authJSON(t *testing.T, refresh, lastRefresh string) string {
	id := jwt(t, `{"sub":"user-1111","email":"masked@example.test","https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`)
	return `{"tokens":{"id_token":"` + id + `","access_token":"at","refresh_token":"` + refresh + `","account_id":"acct-0000"},"last_refresh":"` + lastRefresh + `"}`
}

type env struct {
	home, codexHome string
	cfg             *config.Config
}

func setup(t *testing.T) env {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Codex: config.Codex{
		Homes:         []config.CodexHome{{Name: "default", CodexHome: codexHome}},
		AuthBackupDir: filepath.Join(home, "tmp", ".codex-auths"),
		FullPct:       100, TimeoutS: 5, Binary: "codex",
	}}
	return env{home: home, codexHome: codexHome, cfg: cfg}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestReader(e env, l Launcher) *Reader {
	r := NewReader(e.cfg, nil)
	r.Launcher = l
	r.Home = e.home
	r.Now = func() time.Time { return goldenNow }
	r.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + e.home, "CODEX_THREAD_ID=should-not-leak"}
	return r
}

func ids(refs []model.AccountRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

func TestAccountsDiscovery(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.codexHome, "auth.json"), `{"k":"a"}`)
	write(t, filepath.Join(e.codexHome, "auth_alpha.json"), `{"k":"a"}`)
	write(t, filepath.Join(e.codexHome, "auth-beta.json"), `{"k":"b"}`)
	write(t, filepath.Join(e.codexHome, "authgamma.json"), `{"k":"c"}`)
	write(t, filepath.Join(e.codexHome, "auth.json.lock"), ``)
	write(t, filepath.Join(e.codexHome, "other.json"), `{}`)
	work := filepath.Join(e.home, ".codex-work")
	write(t, filepath.Join(work, "auth_delta.json"), `{"k":"d"}`)
	e.cfg.Codex.Homes = append(e.cfg.Codex.Homes, config.CodexHome{Name: "work", CodexHome: work})
	r := newTestReader(e, nil)

	refs, err := r.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ids(refs), " ")
	want := "codex:beta codex:alpha codex:gamma codex:work/delta" // sorted by file name: auth-beta < auth_alpha
	if got != want {
		t.Fatalf("ids = %q, want %q", got, want)
	}
	if refs[3].Name != "work/delta" || refs[1].Extra[ExtraAlias] != "alpha" {
		t.Fatalf("bad refs: %+v", refs)
	}

	// The bare auth.json joins as "auth" once it matches no snapshot.
	write(t, filepath.Join(e.codexHome, "auth.json"), `{"k":"z"}`)
	refs, _ = r.Accounts(context.Background())
	if got := strings.Join(ids(refs), " "); got != "codex:beta codex:alpha codex:gamma codex:auth codex:work/delta" {
		t.Fatalf("ids = %q", got)
	}
}

func TestActiveAlias(t *testing.T) {
	e := setup(t)
	if _, err := ActiveAlias(e.codexHome); err == nil {
		t.Fatal("want an error without auth.json")
	}
	write(t, filepath.Join(e.codexHome, "auth.json"), `{"k":"b"}`)
	write(t, filepath.Join(e.codexHome, "auth_alpha.json"), `{"k":"a"}`)
	write(t, filepath.Join(e.codexHome, "auth_beta.json"), `{"k":"b"}`)
	write(t, filepath.Join(e.codexHome, "auth_beta2.json"), `{"k":"b"}`)
	if a, err := ActiveAlias(e.codexHome); err != nil || a != "beta" {
		t.Fatalf("ActiveAlias = %q, %v", a, err)
	}
	write(t, filepath.Join(e.codexHome, "auth.json"), `{"k":"q"}`)
	if a, _ := ActiveAlias(e.codexHome); a != "auth" {
		t.Fatalf("ActiveAlias = %q, want auth", a)
	}
}

func TestReadIsolatedSession(t *testing.T) {
	e := setup(t)
	snap := authJSON(t, "r1", "2026-09-01T00:00:00Z")
	write(t, filepath.Join(e.codexHome, "auth.json"), snap)
	write(t, filepath.Join(e.codexHome, "auth_alpha.json"), snap)
	write(t, filepath.Join(e.codexHome, "config.toml"), "model = \"x\"\n")
	l := &fakeLauncher{respond: func(home string) (func(string, *obj) []string, string) {
		// The server refreshes its token: it rewrites the isolated auth.json.
		_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"refreshed":true}`), 0o600)
		return okHandler(rateTwoWindows), ""
	}}
	r := newTestReader(e, l)
	refs, _ := r.Accounts(context.Background())
	if len(refs) != 1 {
		t.Fatalf("refs = %v", ids(refs))
	}
	s, err := r.Read(context.Background(), refs[0], model.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	lc := l.launches[0]
	if strings.Join(lc.argv, " ") != "codex --cd "+filepath.Join(e.home, "tmp")+" app-server" {
		t.Errorf("argv = %v", lc.argv)
	}
	if lc.codexHome == e.codexHome || !strings.Contains(filepath.Base(lc.codexHome), "codex-status-") {
		t.Errorf("CODEX_HOME not isolated: %s", lc.codexHome)
	}
	if envGet(lc.env, "codex_home") != lc.codexHome || envGet(lc.env, "CODEX_THREAD_ID") != "" {
		t.Errorf("env not prepared: %v", lc.env)
	}
	if string(lc.authBytes) != snap || strings.Join(lc.files, ",") != "auth.json,config.toml" {
		t.Errorf("isolated home holds %v", lc.files)
	}
	if _, err := os.Stat(lc.codexHome); !os.IsNotExist(err) {
		t.Errorf("temporary CODEX_HOME left behind")
	}
	if b, _ := os.ReadFile(filepath.Join(e.codexHome, "auth_alpha.json")); string(b) != snap {
		t.Errorf("snapshot was modified")
	}
	if got := strings.Join(l.conns[0].methods, ","); got != "initialize,initialized,account/read,account/rateLimits/read" || !l.conns[0].stopped {
		t.Errorf("methods = %s stopped=%v", got, l.conns[0].stopped)
	}

	if s.ID != "codex:alpha" || s.Verdict != model.Blocked || s.Active == nil || !*s.Active {
		t.Fatalf("snapshot: %+v", s)
	}
	if s.BlockedUntil == nil || *s.BlockedUntil != 1790277200 || strings.Join(s.BlockedReasons, ";") != "Weekly window at 100%" {
		t.Errorf("blocked: %v %v", s.BlockedUntil, s.BlockedReasons)
	}
	if s.Plan == nil || *s.Plan != "plus" || s.Email == nil || *s.Email != "masked@example.test" || s.Label != "alpha" {
		t.Errorf("identity: plan=%v email=%v label=%q", s.Plan, s.Email, s.Label)
	}
	var keys []string
	for _, w := range s.Windows {
		keys = append(keys, w.Key+"/"+w.Scope+"/"+w.Window)
	}
	if got := strings.Join(keys, " "); got != "codex-300/account/5h codex-10080/account/weekly codex_bengalfox-300/model/5h base_model_inference-10080/reserve/weekly" {
		t.Errorf("windows = %s", got)
	}
	var notes []string
	for _, n := range s.Notes {
		notes = append(notes, n.Severity+":"+n.Text)
	}
	if !strings.Contains(strings.Join(notes, "|"), "info:Luna Reserve [gpt-reserve] available") ||
		!strings.Contains(strings.Join(notes, "|"), "warning:app-server rewrote auth.json") {
		t.Errorf("notes = %v", notes)
	}

	// Native is the authFiles row, and replays to the same row.
	var row map[string]any
	if err := json.Unmarshal(s.Native, &row); err != nil {
		t.Fatal(err)
	}
	if row["alias"] != "alpha" || row["authFile"] != "~/.codex/auth_alpha.json" || row["ok"] != true {
		t.Errorf("native row: %v", row)
	}
	if id, _ := row["identity"].(map[string]any); id["workspaceName"] != "alpha" || id["planType"] != "plus" {
		t.Errorf("identity row: %v", row["identity"])
	}
	doc, err := LegacyDocument([]json.RawMessage{s.Native}, false, goldenNow, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(dumpTight(mustParse(t, doc))) != string(s.Native) {
		t.Errorf("replayed row differs:\n%s\n%s", dumpTight(mustParse(t, doc)), s.Native)
	}
	if ok, err := UsableFor(s); err != nil || ok {
		t.Errorf("UsableFor = %v, %v", ok, err)
	}
}

func mustParse(t *testing.T, b []byte) any {
	t.Helper()
	v, err := parseJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReadRecoversUsedRefreshToken(t *testing.T) {
	e := setup(t)
	stale := authJSON(t, "r1", "2026-09-01T00:00:00Z")
	fresh := authJSON(t, "r2", "2026-09-10T00:00:00Z")
	older := authJSON(t, "r0", "2026-08-01T00:00:00Z")
	write(t, filepath.Join(e.codexHome, "auth_alpha.json"), stale)
	write(t, filepath.Join(e.cfg.Codex.AuthBackupDir, "auth_alpha_new.json"), fresh)
	write(t, filepath.Join(e.cfg.Codex.AuthBackupDir, "auth_alpha_old.json"), older)
	write(t, filepath.Join(e.cfg.Codex.AuthBackupDir, "auth_same.json"), stale)
	write(t, filepath.Join(e.cfg.Codex.AuthBackupDir, "auth_other.json"), `{"tokens":{"account_id":"acct-9999","refresh_token":"r9"}}`)
	l := &fakeLauncher{respond: func(home string) (func(string, *obj) []string, string) {
		b, _ := os.ReadFile(filepath.Join(home, "auth.json"))
		if strings.Contains(string(b), `"r1"`) {
			return func(method string, req *obj) []string {
				switch method {
				case "initialize":
					return []string{reply("1", `{}`)}
				case "account/read":
					return []string{reply("2", `{}`)}
				case "account/rateLimits/read":
					return []string{`{"id":"3","error":{"code":-32603,"message":"failed to refresh: refresh token was already used"}}`}
				}
				return nil
			}, "stderr text"
		}
		return okHandler(rateTwoWindows), ""
	}}
	r := newTestReader(e, l)
	refs, _ := r.Accounts(context.Background())
	s, err := r.Read(context.Background(), refs[0], model.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.launches) != 2 || !strings.Contains(string(l.launches[1].authBytes), `"r2"`) {
		t.Fatalf("second launch did not use the newest backup")
	}
	if b, _ := os.ReadFile(filepath.Join(e.codexHome, "auth_alpha.json")); string(b) != fresh {
		t.Errorf("snapshot not replaced by the recovered auth")
	}
	var row struct {
		RecoveredFrom map[string]string `json:"recoveredFrom"`
	}
	_ = json.Unmarshal(s.Native, &row)
	if row.RecoveredFrom["alias"] != "alpha_new" || row.RecoveredFrom["authFile"] != "~/tmp/.codex-auths/auth_alpha_new.json" {
		t.Errorf("recoveredFrom = %v", row.RecoveredFrom)
	}
}

func TestReadFailures(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.codexHome, "auth_alpha.json"), `{"k":"a"}`)
	errorWith := func(msg string) func(string) (func(string, *obj) []string, string) {
		return func(string) (func(string, *obj) []string, string) {
			return func(method string, req *obj) []string {
				switch method {
				case "initialize":
					return []string{reply("1", `{}`)}
				case "account/read":
					return []string{reply("2", `{}`)}
				case "account/rateLimits/read":
					return []string{`{"id":"3","error":{"message":"` + msg + `"}}`}
				}
				return nil
			}, "  trailing stderr  \n"
		}
	}
	ctx := context.Background()

	r := newTestReader(e, &fakeLauncher{respond: errorWith("unexpected status 429 Too Many Requests")})
	refs, _ := r.Accounts(ctx)
	s, err := r.Read(ctx, refs[0], model.ReadOptions{})
	if !model.RateLimited(err) {
		t.Fatalf("want a 429 UpstreamError, got %v", err)
	}
	want := `{"error":"codex-status: account/rateLimits/read failed: {\"message\": \"unexpected status 429 Too Many Requests\"}","stderr":"trailing stderr","ok":false,"alias":"alpha","authFile":"~/.codex/auth_alpha.json"}`
	if string(s.Native) != want {
		t.Errorf("error row:\n got %s\nwant %s", s.Native, want)
	}
	if _, err := UsableFor(s); err == nil {
		t.Errorf("UsableFor of a failed read must error")
	}

	r = newTestReader(e, &fakeLauncher{respond: errorWith("401 Unauthorized")})
	_, err = r.Read(ctx, refs[0], model.ReadOptions{})
	var up *model.UpstreamError
	if err == nil || errors.As(err, &up) {
		t.Errorf("auth failure should be a plain error, got %v", err)
	}

	r = newTestReader(e, &fakeLauncher{err: exec.ErrNotFound})
	_, err = r.Read(ctx, refs[0], model.ReadOptions{})
	if !errors.Is(err, ErrCodexNotFound) {
		t.Errorf("want ErrCodexNotFound, got %v", err)
	}

	// A server that never answers the rate-limit request times out.
	e.cfg.Codex.TimeoutS = 1
	r = newTestReader(e, &fakeLauncher{respond: func(string) (func(string, *obj) []string, string) {
		return func(method string, req *obj) []string {
			if method == "initialize" {
				return []string{reply("1", `{}`)}
			}
			return nil
		}, ""
	}})
	start := time.Now()
	s, err = r.Read(ctx, refs[0], model.ReadOptions{})
	if !errors.As(err, &up) || up.Status != 0 || !strings.Contains(err.Error(), "account/rateLimits/read timed out") {
		t.Errorf("want a timeout UpstreamError, got %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("timeout took %v", d)
	}
	if s.Error == nil || !strings.Contains(*s.Error, "timed out") {
		t.Errorf("snapshot error = %v", s.Error)
	}
}

func TestUsableFor(t *testing.T) {
	// Port of test_codex_quota_verdict.
	cases := []struct {
		native string
		usable bool
		err    bool
	}{
		{`{"ok":true,"quota":{"blocked":false}}`, true, false},
		{`{"ok":true,"quota":{"blocked":true}}`, false, false},
		{`{"ok":false}`, false, true},
		{`{"ok":true,"quota":{}}`, false, true},
		{`{"ok":true}`, false, true},
	}
	for _, c := range cases {
		got, err := UsableFor(model.Snapshot{Native: json.RawMessage(c.native)})
		if (err != nil) != c.err || (err == nil && got != c.usable) {
			t.Errorf("%s: got %v, %v", c.native, got, err)
		}
	}
	if ok, err := UsableFor(model.Snapshot{Verdict: model.Usable}); !ok || err != nil {
		t.Errorf("verdict fallback: %v %v", ok, err)
	}
	if _, err := UsableFor(model.Snapshot{Verdict: model.Unknown}); err == nil {
		t.Errorf("unknown verdict must error")
	}
}

func TestSnapshotFromNative(t *testing.T) {
	row := `{"rateLimits":{"primary":{"usedPercent":64,"windowDurationMins":10080,"resetsAt":1790400000},"secondary":null,"planType":"prolite"},"identity":{"planType":"prolite","workspaceName":"ws"},"ok":true,"alias":"a","authFile":"~/.codex/auth_a.json"}`
	s, err := SnapshotFromNative(json.RawMessage(row), 50, goldenNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.Verdict != model.Blocked || len(s.Windows) != 1 || s.Windows[0].Window != "weekly" || !s.Windows[0].Blocking {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.BlockedReasons[0] != "Weekly window at 64% (>= 50%)" || *s.BlockedUntil != 1790400000 {
		t.Errorf("reasons = %v until %v", s.BlockedReasons, *s.BlockedUntil)
	}
	s, _ = SnapshotFromNative(json.RawMessage(row), 100, goldenNow)
	if s.Verdict != model.Usable || s.Label != "ws" {
		t.Errorf("verdict = %s label = %s", s.Verdict, s.Label)
	}
}
