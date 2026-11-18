package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

const testNow = 1800000000

// fakeSecurity answers `security find-generic-password` from a table keyed
// by "service|account" ("service|" for an unfiltered read).
type fakeSecurity struct {
	mu    sync.Mutex
	items map[string]procx.Result
	calls [][]string
}

func (f *fakeSecurity) Run(ctx context.Context, c procx.Cmd) (procx.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c.Argv)
	service, account := "", ""
	for i := 0; i < len(c.Argv)-1; i++ {
		switch c.Argv[i] {
		case "-s":
			service = c.Argv[i+1]
		case "-a":
			account = c.Argv[i+1]
		}
	}
	res, ok := f.items[service+"|"+account]
	if !ok {
		res = procx.Result{ExitCode: 44}
	}
	if res.ExitCode != 0 {
		return res, errors.New("exit status")
	}
	return res, nil
}

func oauthItem(token string, expiresMs int64, plan string) procx.Result {
	data, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": token, "expiresAt": expiresMs, "subscriptionType": plan}})
	return procx.Result{Stdout: append(data, '\n')}
}

type env map[string]string

func (e env) get(k string) (string, bool) { v, ok := e[k]; return v, ok }

type fixture struct {
	t    *testing.T
	home string
	cfg  *config.Config
	sec  *fakeSecurity
	env  env
	srv  *httptest.Server
	r    *Reader
	// handler answers the usage endpoint.
	handler http.HandlerFunc
	seen    []*http.Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, home: t.TempDir(), sec: &fakeSecurity{items: map[string]procx.Result{}},
		env: env{"USER": "someuser"}}
	f.cfg = &config.Config{Claude: config.Claude{
		Profiles:  []config.ClaudeProfile{{Name: "default"}, {Name: "work", ConfigDir: "~/.claude-work"}},
		UserAgent: "claude-code/0.0.0-test", TokenDir: "~/.keys", TimeoutS: 5, KeychainTimeoutS: 5,
	}}
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"limits":[{"kind":"session","percent":10,"resets_at":1800003600},{"kind":"weekly_all","percent":20,"resets_at":1800300000}]}`))
	}
	var mu sync.Mutex
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		mu.Unlock()
		f.handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	f.r = NewReader(f.cfg, f.sec, f.srv.Client())
	f.r.URL = f.srv.URL
	f.r.Home = f.home
	f.r.GOOS = "darwin"
	f.r.Getenv = f.env.get
	f.r.PasswdName = func() string { return "someuser" }
	f.r.Location = time.UTC
	f.r.Now = func() time.Time { return time.Unix(testNow, 0) }
	return f
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(name string) (model.Snapshot, error) {
	return f.r.Read(context.Background(), model.AccountRef{ID: "claude:" + name, Provider: "claude", Name: name}, model.ReadOptions{})
}

func (f *fixture) workService() string {
	return keychainServiceFor(filepath.Join(f.home, ".claude-work"), func(string) (string, bool) { return "", false })
}

func TestAccounts(t *testing.T) {
	f := newFixture(t)
	refs, _ := f.r.Accounts(context.Background())
	if len(refs) != 2 || refs[0].ID != "claude:default" || refs[1].ID != "claude:work" {
		t.Fatalf("accounts: %+v", refs)
	}
	if _, err := f.read("nope"); !errors.Is(err, model.ErrUnknownAccount) {
		t.Fatalf("unknown account: %v", err)
	}
}

func TestKeychainServiceFor(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	cases := map[string]string{"": "Claude Code-credentials", "/home/u/.claude-work": "Claude Code-credentials-9fa37e90"}
	for dir, want := range cases {
		if got := keychainServiceFor(dir, none); got != want {
			t.Errorf("%q: %s, want %s", dir, got, want)
		}
	}
	sec := func(k string) (string, bool) { return "/sec/dir", k == "CLAUDE_SECURESTORAGE_CONFIG_DIR" }
	if got := keychainServiceFor("", sec); got != "Claude Code-credentials-1fa37d8e" {
		t.Errorf("secure storage override: %s", got)
	}
}

func TestTokenEnvName(t *testing.T) {
	for in, want := range map[string]string{"default": "CLAUDE_CODE_OAUTH_TOKEN_DEFAULT", "work": "CLAUDE_CODE_OAUTH_TOKEN_WORK",
		"my-seat.2": "CLAUDE_CODE_OAUTH_TOKEN_MY_SEAT_2", "--": "CLAUDE_CODE_OAUTH_TOKEN"} {
		if got := tokenEnvName(in); got != want {
			t.Errorf("%q: %s", in, got)
		}
	}
}

func TestTokenOrder(t *testing.T) {
	f := newFixture(t)
	f.cfg.Claude.Profiles[1].TokenEnv = "MY_WORK_TOKEN"
	f.sec.items[f.workService()+"|someuser"] = oauthItem("kc-token", (testNow+3600)*1000, "max")
	f.write(".keys/claude-code-oauth-work", "file-token\n")
	f.env["CLAUDE_CODE_OAUTH_TOKEN_WORK"] = "env-token"
	f.env["MY_WORK_TOKEN"] = "configured-token"
	f.env["CLAUDE_CODE_OAUTH_TOKEN"] = "bare-token"

	steps := []struct{ drop, wantToken, wantSource string }{
		{"", "configured-token", "env:MY_WORK_TOKEN"},
		{"MY_WORK_TOKEN", "env-token", "env:CLAUDE_CODE_OAUTH_TOKEN_WORK"},
		{"CLAUDE_CODE_OAUTH_TOKEN_WORK", "file-token", "token-file:~/.keys/claude-code-oauth-work"},
		{"file", "kc-token", "keychain:someuser"},
	}
	for _, st := range steps {
		if st.drop == "file" {
			os.Remove(filepath.Join(f.home, ".keys", "claude-code-oauth-work"))
		} else if st.drop != "" {
			delete(f.env, st.drop)
		}
		f.seen = nil
		snap, err := f.read("work")
		if err != nil {
			t.Fatalf("%s: %v", st.wantSource, err)
		}
		if got := f.seen[0].Header.Get("Authorization"); got != "Bearer "+st.wantToken {
			t.Errorf("want token %s, sent %q", st.wantToken, got)
		}
		var row legacyRow
		json.Unmarshal(snap.Native, &row)
		if row.TokenSource == nil || *row.TokenSource != st.wantSource {
			t.Errorf("token source %v, want %s", row.TokenSource, st.wantSource)
		}
	}
	// The bare variable is never used: with nothing else, the profile is signed out.
	delete(f.sec.items, f.workService()+"|someuser")
	snap, err := f.read("work")
	if err != nil || snap.Verdict != model.SignedOut {
		t.Fatalf("bare variable must be ignored: verdict %s err %v", snap.Verdict, err)
	}
}

func TestRequestHeaders(t *testing.T) {
	f := newFixture(t)
	f.env["CLAUDE_CODE_OAUTH_TOKEN_DEFAULT"] = "tok"
	snap, err := f.read("default")
	if err != nil {
		t.Fatal(err)
	}
	h := f.seen[0].Header
	if h.Get("anthropic-beta") != "oauth-2025-04-20" || h.Get("User-Agent") != "claude-code/0.0.0-test" ||
		h.Get("Content-Type") != "application/json" || f.seen[0].Method != "GET" {
		t.Errorf("headers: %v", h)
	}
	if snap.Verdict != model.Usable || snap.Source != "api" || snap.Stale || snap.FetchedAt == nil || *snap.FetchedAt != testNow {
		t.Errorf("snapshot: %+v", snap)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].Window != "5h" || *snap.Windows[0].WindowMinutes != 300 ||
		snap.Windows[1].Window != "weekly" || snap.Windows[1].Family != "Claude" || snap.Windows[1].Scope != model.ScopeAccount {
		t.Errorf("windows: %+v", snap.Windows)
	}
	if strings.Contains(string(snap.Native), "tok\"") {
		t.Error("token leaked into the native row")
	}
}

func TestKeychainLookup(t *testing.T) {
	f := newFixture(t)
	f.write("Library/Keychains/aaa.keychain-db", "")
	f.write("Library/Keychains/login.keychain-db", "")
	f.r.PasswdName = func() string { return "pwname" }
	f.env["USER"] = "envname"
	// Only an unfiltered match exists for the default service.
	f.sec.items["Claude Code-credentials|"] = oauthItem("orphan", (testNow+60)*1000, "pro")
	snap, err := f.read("default")
	if err != nil {
		t.Fatal(err)
	}
	var row legacyRow
	json.Unmarshal(snap.Native, &row)
	if *row.TokenSource != "keychain" || row.Keychain.Account != "pwname" || row.Keychain.Service != "Claude Code-credentials" {
		t.Errorf("row: %+v", row)
	}
	if len(row.Warnings) != 1 || !strings.Contains(row.Warnings[0], "no entry for account 'pwname' (this uid's passwd name); used an unfiltered match instead") {
		t.Errorf("warnings: %v", row.Warnings)
	}
	if *snap.Plan != "pro" {
		t.Errorf("plan %v", snap.Plan)
	}
	// Calls: pwname, envname; then the legacy probe: unfiltered, pwname, envname, unknown.
	var accounts []string
	for _, c := range f.sec.calls {
		acc := "<none>"
		for i, a := range c {
			if a == "-a" {
				acc = c[i+1]
			}
		}
		accounts = append(accounts, acc)
		if c[len(c)-2] != filepath.Join(f.home, "Library/Keychains/login.keychain-db") ||
			c[len(c)-1] != filepath.Join(f.home, "Library/Keychains/aaa.keychain-db") || c[len(c)-3] != "-w" {
			t.Errorf("keychains not named login first: %v", c)
		}
	}
	if got := strings.Join(accounts, ","); got != "pwname,envname,<none>,pwname,envname,unknown" {
		t.Errorf("security calls: %s", got)
	}

	// The hashed service of a profile with a config dir never tries "unknown".
	f.sec.calls = nil
	f.read("work")
	for _, c := range f.sec.calls {
		if strings.Contains(strings.Join(c, " "), "unknown") {
			t.Errorf("work profile probed 'unknown': %v", c)
		}
	}
}

func TestBestTokenPrefersLive(t *testing.T) {
	f := newFixture(t)
	f.r.PasswdName = func() string { return "a" }
	f.env["USER"] = "b"
	f.sec.items["Claude Code-credentials|a"] = oauthItem("expired", (testNow-60)*1000, "max")
	f.sec.items["Claude Code-credentials|b"] = oauthItem("live", (testNow+60)*1000, "max")
	f.read("default")
	if got := f.seen[0].Header.Get("Authorization"); got != "Bearer live" {
		t.Errorf("sent %s", got)
	}
}

func TestKeychainNoGUIIsAnErrorNotSignedOut(t *testing.T) {
	f := newFixture(t)
	f.sec.items[f.workService()+"|someuser"] = procx.Result{ExitCode: 36, Stderr: []byte("whatever")}
	snap, err := f.read("work")
	if err == nil || snap.Verdict != model.Unknown || snap.Source != "" || snap.Error == nil {
		t.Fatalf("verdict %s source %q err %v", snap.Verdict, snap.Source, err)
	}
	if !strings.Contains(err.Error(), "security exit 36") || !strings.Contains(err.Error(), "could not read the Keychain credential for account 'someuser'") {
		t.Errorf("message: %v", err)
	}
	var row map[string]any
	json.Unmarshal(snap.Native, &row)
	if row["error"] == nil || row["keychain"] == nil {
		t.Errorf("native error row: %s", snap.Native)
	}
	if len(f.seen) != 0 {
		t.Error("fetched without a token")
	}

	// Other exit codes name stderr's last line; a timeout says so.
	f.sec.items[f.workService()+"|someuser"] = procx.Result{ExitCode: 51, Stderr: []byte("first\nlast line\n")}
	_, err = f.read("work")
	if err == nil || !strings.Contains(err.Error(), "(security exit 51): last line") {
		t.Errorf("exit 51: %v", err)
	}
}

func TestKeychainTimeout(t *testing.T) {
	f := newFixture(t)
	f.r.run = runnerFunc(func(context.Context, procx.Cmd) (procx.Result, error) {
		return procx.Result{ExitCode: -1}, procx.ErrTimeout
	})
	_, err := f.read("default")
	if err == nil || !strings.Contains(err.Error(), "timed out after 5s reading Keychain item 'Claude Code-credentials'") {
		t.Errorf("timeout: %v", err)
	}
}

type runnerFunc func(context.Context, procx.Cmd) (procx.Result, error)

func (r runnerFunc) Run(ctx context.Context, c procx.Cmd) (procx.Result, error) { return r(ctx, c) }

func TestCredentialsFile(t *testing.T) {
	f := newFixture(t)
	f.r.GOOS = "linux" // no Keychain
	f.write(".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"filetok","expiresAt":1900000000000,"subscriptionType":"max"}}`)
	snap, err := f.read("work")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.seen[0].Header.Get("Authorization"); got != "Bearer filetok" {
		t.Errorf("sent %s", got)
	}
	var row legacyRow
	json.Unmarshal(snap.Native, &row)
	if !strings.HasPrefix(*row.TokenSource, "credentials-file:") || *row.TokenExpired {
		t.Errorf("row %+v", row)
	}
	if len(f.sec.calls) != 0 {
		t.Error("Keychain consulted off darwin")
	}
}

func TestSignedOut(t *testing.T) {
	f := newFixture(t)
	snap, err := f.read("work")
	if err != nil || snap.Verdict != model.SignedOut || snap.Error != nil {
		t.Fatalf("verdict %s err %v", snap.Verdict, err)
	}
	var row map[string]any
	json.Unmarshal(snap.Native, &row)
	msg, _ := row["error"].(string)
	if !strings.HasPrefix(msg, "no OAuth token found (tried CLAUDE_CODE_OAUTH_TOKEN_WORK, ~/.keys/claude-code-oauth-work, Keychain item") {
		t.Errorf("message %q", msg)
	}
	if row["profile"] != "work" {
		t.Errorf("row %v", row)
	}
}

const configCache = `{"numStartups": 3, "cachedUsageUtilization": {"fetchedAtMs": 1799996400000, "utilization":
 {"five_hour": {"utilization": 100.0, "resets_at": "2027-01-15T07:00:00Z"}, "seven_day": {"utilization": 40, "resets_at": 1800300000}}}}`

func TestConfigCacheFallbackWithoutToken(t *testing.T) {
	f := newFixture(t)
	f.write(".claude-work/.claude.json", configCache)
	snap, err := f.read("work")
	if err == nil || snap.Source != "config-cache" || !snap.Stale || snap.Error == nil || snap.StaleReason == nil {
		t.Fatalf("fallback: %+v err %v", snap, err)
	}
	// The 5h window is at 100% but its reset has passed: rolled over, so
	// not blocking; the weekly one decides.
	if snap.Verdict != model.Usable || snap.Windows[0].Blocking {
		t.Errorf("verdict %s windows %+v", snap.Verdict, snap.Windows)
	}
	var row legacyRow
	json.Unmarshal(snap.Native, &row)
	if row.Source != "config-cache" || !row.Cache.FromCache || row.Cache.StaleReason == nil || *row.Cache.FetchedAt != 1799996400 || row.TokenSource != nil {
		t.Errorf("row %+v", row)
	}
	// The default profile's config lives at ~/.claude.json.
	f.write(".claude.json", configCache)
	if snap, _ := f.read("default"); snap.Source != "config-cache" {
		t.Errorf("default profile config cache: %s", snap.Source)
	}
}

func TestUpstreamErrors(t *testing.T) {
	f := newFixture(t)
	f.env["CLAUDE_CODE_OAUTH_TOKEN_WORK"] = "tok"

	f.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}
	snap, err := f.read("work")
	var up *model.UpstreamError
	if !errors.As(err, &up) || up.Status != 429 || up.RetryAfter != 120*time.Second || !model.RateLimited(err) {
		t.Fatalf("429: %v", err)
	}
	if snap.Source != "" || snap.Error == nil || !strings.HasPrefix(*snap.Error, "usage endpoint returned HTTP 429 (rate limited;") {
		t.Errorf("429 snapshot: %+v", snap)
	}

	// With a config cache, the snapshot carries it and the error.
	f.write(".claude-work/.claude.json", configCache)
	snap, err = f.read("work")
	if !model.RateLimited(err) || snap.Source != "config-cache" || !strings.Contains(*snap.StaleReason, "HTTP 429") {
		t.Errorf("429 with cache: %v %+v", err, snap)
	}

	f.handler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }
	_, err = f.read("work")
	if !errors.As(err, &up) || up.Status != 503 || up.RetryAfter != 0 {
		t.Errorf("503: %v", err)
	}

	f.handler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); w.Write([]byte("nope")) }
	_, err = f.read("work")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 401 || errors.As(err, &up) ||
		se.Msg != "usage endpoint returned HTTP 401 (token rejected); "+loginHint+": nope" {
		t.Errorf("401: %v", err)
	}

	f.handler = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("[1]")) }
	if _, err = f.read("work"); err == nil || err.Error() != "usage endpoint returned non-object JSON: [1]" {
		t.Errorf("non-object: %v", err)
	}
	f.handler = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) }
	if _, err = f.read("work"); err == nil || err.Error() != "usage endpoint returned invalid JSON: <html>" {
		t.Errorf("invalid: %v", err)
	}

	f.r.URL = "http://127.0.0.1:1/unreachable"
	_, err = f.read("work")
	if !errors.As(err, &up) || up.Status != 0 || !strings.HasPrefix(reason(err), "usage endpoint unreachable: ") {
		t.Errorf("transport: %v", err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Unix(testNow, 0)
	if d := parseRetryAfter(now.Add(90*time.Second).UTC().Format(http.TimeFormat), now); d != 90*time.Second {
		t.Errorf("date: %v", d)
	}
	if d := parseRetryAfter("soon", now); d != 0 {
		t.Errorf("junk: %v", d)
	}
}

func TestReadOptionsClock(t *testing.T) {
	f := newFixture(t)
	f.env["CLAUDE_CODE_OAUTH_TOKEN_WORK"] = "tok"
	snap, _ := f.r.Read(context.Background(), model.AccountRef{Name: "work"},
		model.ReadOptions{Now: func() time.Time { return time.Unix(1800000500, 0) }})
	if *snap.FetchedAt != 1800000500 {
		t.Errorf("fetched_at %v", *snap.FetchedAt)
	}
}
