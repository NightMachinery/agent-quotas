package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// The golden files come from the old zsh agy-status on the same synthetic
// accounts, cache and /usage output (testdata/gen/legacy.zsh).

// fakeAgy answers `agy -p /usage` from testdata/usage.txt, and fails with
// exit 3 for the account named broke.
type fakeAgy struct {
	mu    sync.Mutex
	calls []procx.Cmd
	exit  map[string]int
	out   []byte
	sleep time.Duration
}

func (f *fakeAgy) Run(ctx context.Context, c procx.Cmd) (procx.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	account := envValue(c.Env, "AGY_ACCOUNT_ID")
	if code := f.exit[account]; code != 0 {
		return procx.Result{ExitCode: code}, errors.New("agy: exit status 3")
	}
	if f.sleep > 0 {
		return procx.Result{ExitCode: -1}, procx.ErrTimeout
	}
	return procx.Result{Stdout: f.out}, nil
}

func (f *fakeAgy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func envValue(env []string, name string) string {
	v := ""
	for _, kv := range env {
		if rest, ok := strings.CutPrefix(kv, name+"="); ok {
			v = rest
		}
	}
	return v
}

type fixtureEnv struct {
	dir    string
	cfg    *config.Config
	agy    *fakeAgy
	reader *Reader
}

func newFixture(t *testing.T) *fixtureEnv {
	t.Helper()
	dir := t.TempDir()
	usage, err := os.ReadFile(filepath.Join("testdata", "usage.txt"))
	if err != nil {
		t.Fatal(err)
	}
	maxAge := -1
	cfg := &config.Config{Agy: config.Agy{
		Accounts: []config.AgyAccount{
			{Name: "m", GeminiDir: filepath.Join(dir, "gemini"), CacheFile: filepath.Join(dir, "main-cache.json")},
			{Name: "cave", Label: "Cave lab", GeminiDir: filepath.Join(dir, "accounts", "cave")},
			{Name: "fm2", GeminiDir: filepath.Join(dir, "accounts", "fm2")},
			{Name: "broke", GeminiDir: filepath.Join(dir, "accounts", "broke")},
		},
		StatuslineMaxAgeS: &maxAge, MinIntervalS: 60, SlowFallbackS: 60, TimeoutS: 90, Binary: "agy",
	}}
	for _, name := range []string{"m", "cave", "broke"} {
		a := ResolveAccount(cfg.Agy.Accounts[indexOf(cfg, name)])
		if err := os.MkdirAll(a.CLIDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(a.TokenFile(), []byte("inert fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "fm2", "antigravity-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	cache, err := os.ReadFile(filepath.Join("testdata", "m-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main-cache.json"), cache, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgy{exit: map[string]int{"broke": 3}, out: usage}
	r := NewReader(cfg, fake)
	r.Location = time.UTC
	return &fixtureEnv{dir, cfg, fake, r}
}

func indexOf(cfg *config.Config, name string) int {
	for i, a := range cfg.Agy.Accounts {
		if a.Name == name {
			return i
		}
	}
	return -1
}

func ref(name string) model.AccountRef {
	return model.AccountRef{ID: model.ID(model.Agy, name), Provider: model.Agy, Name: name}
}

func at(epoch int64) model.ReadOptions {
	return model.ReadOptions{Now: func() time.Time { return time.Unix(epoch, 0) }}
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// slowCapture is the capture time the zsh run stamped into a merged cache.
func slowCapture(t *testing.T, name string) int64 {
	var doc struct {
		Buckets map[string]json.RawMessage `json:"buckets"`
	}
	var bucket struct {
		CapturedAt int64 `json:"captured_at"`
	}
	if err := json.Unmarshal(golden(t, name), &doc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(doc.Buckets["gemini-weekly"], &bucket); err != nil {
		t.Fatal(err)
	}
	return bucket.CapturedAt
}

func sameBytes(t *testing.T, name string, got []byte) {
	t.Helper()
	if want := golden(t, name); !bytes.Equal(got, want) {
		t.Errorf("%s differs\n got: %s\nwant: %s", name, got, want)
	}
}

func TestLegacyGolden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	caveNow := slowCapture(t, "cave-cache-after.json")
	var rows []json.RawMessage
	for _, name := range []string{"m", "cave", "fm2", "broke"} {
		snap, err := f.reader.Read(ctx, ref(name), at(caveNow))
		if (err != nil) != (name == "broke") {
			t.Fatalf("%s: err = %v", name, err)
		}
		rows = append(rows, snap.Native)
	}
	sameBytes(t, "legacy-normal.json", LegacyJSON(rows))
	if ExitCode(rows) != 1 {
		t.Error("an account error should make the exit status 1")
	}
	cave := ResolveAccount(f.cfg.Agy.Accounts[1])
	got, err := os.ReadFile(cave.CacheFile)
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "cave-cache-after.json", got)

	mNow := slowCapture(t, "m-cache-after.json")
	opts := at(mNow)
	opts.Force = true
	snap, err := f.reader.Read(ctx, ref("m"), opts)
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "legacy-forced.json", LegacyJSON([]json.RawMessage{snap.Native}))
	got, err = os.ReadFile(filepath.Join(f.dir, "main-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "m-cache-after.json", got)
}

func TestSlowPathCommand(t *testing.T) {
	f := newFixture(t)
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-session")
	opts := at(2000000000)
	opts.Force = true
	if _, err := f.reader.Read(context.Background(), ref("cave"), opts); err != nil {
		t.Fatal(err)
	}
	c := f.agy.calls[0]
	gemini := ResolveAccount(f.cfg.Agy.Accounts[1]).GeminiDir
	want := []string{"agy", "--gemini_dir=" + gemini, "--app_data_dir=antigravity-cli", "--mode", "plan", "--print", "/usage"}
	if strings.Join(c.Argv, " ") != strings.Join(want, " ") {
		t.Errorf("argv %q", c.Argv)
	}
	for name, want := range map[string]string{
		"ANTIGRAVITY_GEMINI_DIR": gemini, "AGY_ACCOUNT_ID": "cave", "AGY_ACCOUNT_LABEL": "Cave lab",
		"SSH_CONNECTION": "agy-file-auth", "CLAUDE_CODE_SESSION_ID": "",
		"agy_status_cache_file": filepath.Join(gemini, "antigravity-cli", "quota-cache.json"),
	} {
		if got := envValue(c.Env, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if c.Timeout != 90*time.Second || c.Dir == "" || c.Stdin != nil {
		t.Errorf("timeout %v dir %q stdin %v", c.Timeout, c.Dir, c.Stdin)
	}
	if _, err := os.Stat(c.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("the throwaway working directory should be gone")
	}
}

func TestSnapshotVerdicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// m's cache: 3p-weekly at 0% (undated), Gemini pools have room.
	snap, err := f.reader.Read(ctx, ref("m"), at(1999990400))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Verdict != model.Usable || snap.Source != "statusline" || snap.Plan == nil || *snap.Plan != "Google AI Pro" {
		t.Errorf("m: %+v", snap)
	}
	if snap.FetchedAt == nil || *snap.FetchedAt != 1999990000 {
		t.Errorf("fetched_at should be the oldest capture: %v", snap.FetchedAt)
	}
	scopes := map[string]string{}
	for _, w := range snap.Windows {
		scopes[w.Key] = w.Scope
		if w.Key == "3p-weekly" && (!w.Blocking || *w.UsedPercent != 100 || w.Family != "Claude/GPT" || w.Window != "weekly") {
			t.Errorf("3p-weekly window: %+v", w)
		}
	}
	if scopes["gemini-5h"] != model.ScopeAccount || scopes["gemini-4-argon-weekly"] != model.ScopeModel {
		t.Errorf("scopes %v", scopes)
	}
	if !strings.Contains(snap.Notes[0].Text, "Cache 6m old") || !strings.Contains(snap.Notes[0].Text, "some rows undated") {
		t.Errorf("provenance note %q", snap.Notes[0].Text)
	}

	// Every family exhausted: blocked until the first family is back.
	writeCache(t, f, `{"buckets":{
	 "gemini-5h":{"remaining_percent":0,"resets_at":2000000500,"captured_at":1999990000},
	 "gemini-weekly":{"remaining_percent":0,"resets_at":2000009000,"captured_at":1999990000},
	 "3p-weekly":{"remaining_percent":0,"resets_at":2000004000,"captured_at":1999990000},
	 "gemini-4-argon-weekly":{"remaining_percent":50,"resets_at":2000000100,"captured_at":1999990000}}}`)
	snap, err = f.reader.Read(ctx, ref("m"), at(1999990400))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Verdict != model.Blocked || snap.BlockedUntil == nil || *snap.BlockedUntil != 2000004000 || len(snap.BlockedReasons) != 3 {
		t.Errorf("blocked: %v until %v reasons %v", snap.Verdict, snap.BlockedUntil, snap.BlockedReasons)
	}
	// After the 3p reset the reading predates it: no longer blocking.
	snap, _ = f.reader.Read(ctx, ref("m"), at(2000004100))
	if snap.Verdict != model.Usable || !snap.Stale {
		t.Errorf("after reset: %v stale %v", snap.Verdict, snap.Stale)
	}
	// A model pool alone does not block the account.
	writeCache(t, f, `{"buckets":{"gemini-weekly":{"remaining_percent":40,"captured_at":1},"gemini-4-argon-weekly":{"remaining_percent":0,"captured_at":1}}}`)
	if snap, _ = f.reader.Read(ctx, ref("m"), at(1999990400)); snap.Verdict != model.Usable {
		t.Errorf("model pool: %v", snap.Verdict)
	}
	// Signed out is a status, not an error.
	snap, err = f.reader.Read(ctx, ref("fm2"), at(1999990400))
	if err != nil || snap.Verdict != model.SignedOut {
		t.Errorf("fm2: %v %v", snap.Verdict, err)
	}
	if _, err := f.reader.Read(ctx, ref("nobody"), at(1)); !errors.Is(err, model.ErrUnknownAccount) {
		t.Errorf("unknown account: %v", err)
	}
}

func writeCache(t *testing.T, f *fixtureEnv, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "main-cache.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFallbackRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// cave has no cache: the slow path runs once, then not again within
	// slow_fallback_s, because the cache it writes now serves.
	if _, err := f.reader.Read(ctx, ref("cave"), at(1000)); err != nil || f.agy.count() != 1 {
		t.Fatalf("first read: %v, %d calls", err, f.agy.count())
	}
	os.Remove(ResolveAccount(f.cfg.Agy.Accounts[1]).CacheFile)
	if _, err := f.reader.Read(ctx, ref("cave"), at(1030)); err == nil || f.agy.count() != 1 {
		t.Errorf("guarded fallback ran agy again: %v, %d calls", err, f.agy.count())
	}
	if _, err := f.reader.Read(ctx, ref("cave"), at(1061)); err != nil || f.agy.count() != 2 {
		t.Errorf("fallback after slow_fallback_s: %v, %d calls", err, f.agy.count())
	}
	// Max age drops old buckets; an all-old cache falls back.
	maxAge := 3600
	f.cfg.Agy.StatuslineMaxAgeS = &maxAge
	snap, err := f.reader.Read(ctx, ref("m"), at(1999990000+3600+150))
	if err != nil || snap.Source != "statusline" || len(snap.Windows) != 2 {
		t.Errorf("max age: %v %s %d windows", err, snap.Source, len(snap.Windows))
	}
	// Cache-only never runs agy.
	f.reader.CacheOnly = true
	os.Remove(ResolveAccount(f.cfg.Agy.Accounts[1]).CacheFile)
	snap, err = f.reader.Read(ctx, ref("cave"), at(5000))
	if err == nil || f.agy.count() != 2 || snap.Verdict != model.Unknown {
		t.Errorf("cache only: %v, %d calls, %s", err, f.agy.count(), snap.Verdict)
	}
	var legacy LegacyAccount
	if json.Unmarshal(snap.Native, &legacy) != nil || legacy.Error == nil || !strings.Contains(*legacy.Error, "exit 1") {
		t.Errorf("cache-only native: %s", snap.Native)
	}
	// A timeout is exit 124 and an upstream error the daemon backs off from.
	f.reader.CacheOnly = false
	f.agy.sleep = time.Second
	opts := at(9000)
	opts.Force = true
	snap, err = f.reader.Read(ctx, ref("cave"), opts)
	var up *model.UpstreamError
	if !errors.As(err, &up) || !strings.Contains(string(snap.Native), "exit 124") {
		t.Errorf("timeout: %v %s", err, snap.Native)
	}
}

func TestCacheReadsNeverOpenTheToken(t *testing.T) {
	f := newFixture(t)
	a := ResolveAccount(f.cfg.Agy.Accounts[0])
	// An unreadable token still counts as present: only its size is looked at.
	if err := os.Chmod(a.TokenFile(), 0o000); err != nil {
		t.Fatal(err)
	}
	if !a.SignedIn() {
		t.Error("token presence should not need read access")
	}
	if err := os.Chmod(a.TokenFile(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.TokenFile(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if a.SignedIn() {
		t.Error("an empty token file is not signed in")
	}
}

// keychainAgy answers the keychain lookup with exit; anything else is agy.
type keychainAgy struct {
	fakeAgy
	keychain int
}

func (k *keychainAgy) Run(ctx context.Context, c procx.Cmd) (procx.Result, error) {
	if len(c.Argv) > 0 && c.Argv[0] == keychainArgv[0] {
		k.mu.Lock()
		k.calls = append(k.calls, c)
		k.mu.Unlock()
		if strings.Contains(strings.Join(c.Argv, " "), " -w") || strings.Contains(strings.Join(c.Argv, " "), " -g") {
			return procx.Result{ExitCode: 1}, errors.New("the keychain lookup must never ask for the secret")
		}
		if k.keychain != 0 {
			return procx.Result{ExitCode: k.keychain}, errors.New("security: exit status 44")
		}
		return procx.Result{}, nil
	}
	return k.fakeAgy.Run(ctx, c)
}

// agy signs account m in from the keychain when it has no token file, so
// for the account in the default ~/.gemini a keychain item is a sign-in.
func TestKeychainSignInCountsForTheDefaultRoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the keychain is macOS only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	usage, err := os.ReadFile(filepath.Join("testdata", "usage.txt"))
	if err != nil {
		t.Fatal(err)
	}
	maxAge := -1
	cfg := &config.Config{Agy: config.Agy{
		Accounts: []config.AgyAccount{
			{Name: "m", CacheFile: filepath.Join(home, "main-cache.json")},
			{Name: "fm2", GeminiDir: filepath.Join(home, "accounts", "fm2")},
		},
		StatuslineMaxAgeS: &maxAge, MinIntervalS: 60, SlowFallbackS: 60, TimeoutS: 90, Binary: "agy",
	}}
	for _, c := range cfg.Agy.Accounts {
		if err := os.MkdirAll(ResolveAccount(c).CLIDir(), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if a := ResolveAccount(cfg.Agy.Accounts[0]); !a.Keychain {
		t.Fatalf("m in ~/.gemini should use the keychain: %+v", a)
	}
	if a := ResolveAccount(cfg.Agy.Accounts[1]); a.Keychain {
		t.Fatalf("an account in its own root must not: %+v", a)
	}
	fake := &keychainAgy{fakeAgy: fakeAgy{out: usage}}
	r := NewReader(cfg, fake)
	r.Location = time.UTC
	ctx := context.Background()

	snap, err := r.Read(ctx, ref("m"), at(9000))
	// No cache here, so the live check runs and verifies the sign-in.
	if err != nil || !strings.Contains(string(snap.Native), `"signed_in":true`) || fake.calls[0].Argv[0] != keychainArgv[0] {
		t.Fatalf("keychain sign-in: %v %s", err, snap.Native)
	}
	fake.keychain = 44
	snap, _ = r.Read(ctx, ref("m"), at(9100))
	if !strings.Contains(string(snap.Native), "Not signed in; run agy-acc-login m") {
		t.Fatalf("no keychain item, no file: %s", snap.Native)
	}
	fake.keychain = 0
	snap, _ = r.Read(ctx, ref("fm2"), at(9200))
	if !strings.Contains(string(snap.Native), "Not signed in; run agy-acc-login fm2") {
		t.Fatalf("fm2 must not borrow the keychain sign-in: %s", snap.Native)
	}

	// StoredAuth, which agy-auth prints for the shell: the file wins without
	// asking the keychain, and fm2 never asks it.
	m, fm2 := ResolveAccount(cfg.Agy.Accounts[0]), ResolveAccount(cfg.Agy.Accounts[1])
	if got := StoredAuth(ctx, fake, m); got != AuthKeychain {
		t.Fatalf("m with only a keychain item: %s", got)
	}
	if got := StoredAuth(ctx, fake, fm2); got != AuthMissing {
		t.Fatalf("fm2 with nothing: %s", got)
	}
	if err := os.WriteFile(fm2.TokenFile(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	if got := StoredAuth(ctx, fake, fm2); got != AuthFile || len(fake.calls) != 0 {
		t.Fatalf("fm2 with a token file: %s, %d calls", got, len(fake.calls))
	}
}
