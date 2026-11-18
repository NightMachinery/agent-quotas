package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/client"
	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
)

type fakeReader struct {
	provider string
	names    []string
	calls    atomic.Int32
	delay    time.Duration
	mu       sync.Mutex
	next     func(ref model.AccountRef, force bool) (model.Snapshot, error)
}

func (f *fakeReader) Provider() string { return f.provider }

func (f *fakeReader) Accounts(context.Context) ([]model.AccountRef, error) {
	var out []model.AccountRef
	for _, n := range f.names {
		out = append(out, model.AccountRef{ID: model.ID(f.provider, n), Provider: f.provider, Name: n})
	}
	return out, nil
}

func (f *fakeReader) Read(ctx context.Context, ref model.AccountRef, opts model.ReadOptions) (model.Snapshot, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	next := f.next
	f.mu.Unlock()
	return next(ref, opts.Force)
}

func (f *fakeReader) set(fn func(ref model.AccountRef, force bool) (model.Snapshot, error)) {
	f.mu.Lock()
	f.next = fn
	f.mu.Unlock()
}

func usable(ref model.AccountRef, _ bool) (model.Snapshot, error) {
	return model.Snapshot{Verdict: model.Usable, Source: "api", FetchedAt: model.F(model.Unix(time.Now()))}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newTestDaemon(t *testing.T, readers ...model.Reader) (*Daemon, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	d := New(testConfig(t), readers, Options{StateDir: t.TempDir(), Now: c.now,
		Logger: log.New(io.Discard, "", 0)})
	if err := d.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d, c
}

func TestForcedRefreshCoalescesAndFloors(t *testing.T) {
	r := &fakeReader{provider: model.Claude, names: []string{"work"}, delay: 50 * time.Millisecond}
	r.set(usable)
	d, c := newTestDaemon(t, r)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Get(context.Background(), "claude:work", true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("concurrent forced refreshes made %d upstream calls, want 1", n)
	}
	// Within the floor: no new call.
	c.add(2 * time.Second)
	if _, err := d.Get(context.Background(), "claude:work", true); err != nil {
		t.Fatal(err)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("forced refresh inside the floor called upstream (%d calls)", n)
	}
	// After the floor: one more.
	c.add(10 * time.Second)
	if _, err := d.Get(context.Background(), "claude:work", true); err != nil {
		t.Fatal(err)
	}
	if n := r.calls.Load(); n != 2 {
		t.Fatalf("forced refresh after the floor: %d calls, want 2", n)
	}
}

func TestBackoffOn429KeepsDataAndRefusesEarlyForce(t *testing.T) {
	r := &fakeReader{provider: model.Claude, names: []string{"work"}}
	r.set(usable)
	d, c := newTestDaemon(t, r)
	if _, err := d.Get(context.Background(), "claude:work", true); err != nil {
		t.Fatal(err)
	}
	r.set(func(model.AccountRef, bool) (model.Snapshot, error) {
		return model.Snapshot{}, &model.UpstreamError{Status: 429, RetryAfter: 120 * time.Second, Err: errors.New("rate limited")}
	})
	c.add(10 * time.Second)
	s, err := d.Get(context.Background(), "claude:work", true)
	if err != nil {
		t.Fatal(err)
	}
	if s.Verdict != model.Usable || !s.Stale || s.Error == nil {
		t.Fatalf("after 429: verdict %s stale %v error %v; want the old usable reading marked stale", s.Verdict, s.Stale, s.Error)
	}
	if s.Poll.BackoffUntil == nil || s.Poll.LastHTTPStatus != 429 {
		t.Fatalf("back-off not recorded: %+v", s.Poll)
	}
	c.add(30 * time.Second)
	calls := r.calls.Load()
	if _, err := d.Get(context.Background(), "claude:work", true); !errors.Is(err, ErrBackedOff) {
		t.Fatalf("forced refresh during back-off: err %v, want ErrBackedOff", err)
	}
	if r.calls.Load() != calls {
		t.Fatal("forced refresh during back-off reached upstream")
	}
	c.add(100 * time.Second)
	r.set(usable)
	if _, err := d.Get(context.Background(), "claude:work", true); err != nil {
		t.Fatal(err)
	}
	s, _ = d.Get(context.Background(), "claude:work", false)
	if s.Stale || s.Error != nil || s.Poll.ConsecutiveFailures != 0 {
		t.Fatalf("after recovery: %+v", s)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	d, _ := newTestDaemon(t)
	if got := d.backoff(1, nil); got != time.Minute {
		t.Fatalf("first back-off %v", got)
	}
	if got := d.backoff(3, nil); got != 4*time.Minute {
		t.Fatalf("third back-off %v", got)
	}
	if got := d.backoff(30, nil); got != 30*time.Minute {
		t.Fatalf("capped back-off %v", got)
	}
	if got := d.backoff(1, &model.UpstreamError{Status: 429, RetryAfter: 10 * time.Minute}); got != 10*time.Minute {
		t.Fatalf("Retry-After ignored: %v", got)
	}
}

func TestFallbackSnapshotUsedOnlyWhenNewer(t *testing.T) {
	r := &fakeReader{provider: model.Claude, names: []string{"default"}}
	old := model.Unix(time.Unix(1_799_990_000, 0))
	r.set(func(model.AccountRef, bool) (model.Snapshot, error) {
		return model.Snapshot{Verdict: model.Blocked, Source: "config-cache", FetchedAt: model.F(old)},
			errors.New("no token")
	})
	d, c := newTestDaemon(t, r)
	s, _ := d.Get(context.Background(), "claude:default", true)
	if s.Source != "config-cache" || s.Verdict != model.Blocked || s.Error == nil {
		t.Fatalf("first fallback not used: %+v", s)
	}
	newer := model.Unix(c.now())
	r.set(func(model.AccountRef, bool) (model.Snapshot, error) {
		return model.Snapshot{Verdict: model.Usable, Source: "api", FetchedAt: model.F(newer)}, nil
	})
	c.add(10 * time.Second)
	if s, _ = d.Get(context.Background(), "claude:default", true); s.Source != "api" {
		t.Fatalf("live reading not used: %+v", s)
	}
	r.set(func(model.AccountRef, bool) (model.Snapshot, error) {
		return model.Snapshot{Verdict: model.Blocked, Source: "config-cache", FetchedAt: model.F(old)},
			errors.New("no token")
	})
	c.add(10 * time.Second)
	if s, _ = d.Get(context.Background(), "claude:default", true); s.Source != "api" || s.Verdict != model.Usable || !s.Stale {
		t.Fatalf("older fallback replaced a newer reading: %+v", s)
	}
}

func TestTransitionsNotifySubscribers(t *testing.T) {
	r := &fakeReader{provider: model.Codex, names: []string{"a"}}
	r.set(func(model.AccountRef, bool) (model.Snapshot, error) {
		return model.Snapshot{Verdict: model.Blocked, BlockedReasons: []string{"weekly"}}, nil
	})
	d, c := newTestDaemon(t, r)
	got := make(chan model.Transition, 4)
	d.Subscribe(func(tr model.Transition, s model.Snapshot) { got <- tr })
	d.Get(context.Background(), "codex:a", true)
	r.set(usable)
	c.add(10 * time.Second)
	d.Get(context.Background(), "codex:a", true)
	want := [][2]string{{model.Unknown, model.Blocked}, {model.Blocked, model.Usable}}
	for _, w := range want {
		select {
		case tr := <-got:
			if tr.From != w[0] || tr.To != w[1] {
				t.Fatalf("transition %s->%s, want %s->%s", tr.From, tr.To, w[0], w[1])
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no transition %v", w)
		}
	}
	ts, err := d.Transitions(0)
	if err != nil || len(ts) != 2 {
		t.Fatalf("recorded transitions: %v %v", ts, err)
	}
}

func TestNextPollWakesAtBlockedReset(t *testing.T) {
	r := &fakeReader{provider: model.Claude, names: []string{"work"}}
	d, c := newTestDaemon(t, r)
	reset := c.now().Add(10 * time.Minute)
	r.set(func(model.AccountRef, bool) (model.Snapshot, error) {
		return model.Snapshot{Verdict: model.Blocked, BlockedUntil: model.F(model.Unix(reset))}, nil
	})
	d.Get(context.Background(), "claude:work", true)
	a := d.accounts["claude:work"]
	next := d.nextPoll(a)
	want := reset.Add(90 * time.Second)
	if next.After(want) || next.Before(c.now()) {
		t.Fatalf("next poll %v, want at most %v", next, want)
	}
	if !next.Equal(c.now().Add(5*time.Minute)) && !next.Equal(want) {
		t.Fatalf("next poll %v is neither the interval nor the reset", next)
	}
}

func TestPersistAndRestore(t *testing.T) {
	dir := t.TempDir()
	r := &fakeReader{provider: model.Agy, names: []string{"m"}}
	r.set(usable)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	cfg := testConfig(t)
	d := New(cfg, []model.Reader{r}, Options{StateDir: dir, Now: c.now, Logger: log.New(io.Discard, "", 0)})
	d.Discover(context.Background())
	d.Get(context.Background(), "agy:m", true)
	d2 := New(cfg, []model.Reader{r}, Options{StateDir: dir, Now: c.now, Logger: log.New(io.Discard, "", 0)})
	d2.loadSnapshots()
	d2.Discover(context.Background())
	s, _ := d2.Get(context.Background(), "agy:m", false)
	if s.Verdict != model.Usable || s.Source != "snapshot-file" {
		t.Fatalf("restored %+v", s)
	}
	if fi, err := os.Stat(filepath.Join(dir, "snapshots.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("snapshots.json mode: %v %v", fi, err)
	}
}

func TestResolve(t *testing.T) {
	a := &fakeReader{provider: model.Claude, names: []string{"default", "work"}}
	b := &fakeReader{provider: model.Codex, names: []string{"x"}}
	d, _ := newTestDaemon(t, b, a)
	ids, err := d.Resolve(nil)
	if err != nil || len(ids) != 3 || ids[0] != "claude:default" || ids[2] != "codex:x" {
		t.Fatalf("all: %v %v", ids, err)
	}
	if ids, _ = d.Resolve([]string{"claude"}); len(ids) != 2 {
		t.Fatalf("provider: %v", ids)
	}
	if _, err = d.Resolve([]string{"claude:nope"}); !errors.Is(err, model.ErrUnknownAccount) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestSocketRoundTrip(t *testing.T) {
	r := &fakeReader{provider: model.Claude, names: []string{"work"}}
	r.set(usable)
	d, _ := newTestDaemon(t, r)
	dir, err := os.MkdirTemp("/tmp", "aq-sock-") // short path: unix socket names are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "s.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o", fi.Mode().Perm())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewServer(d).Serve(ctx, l)
	cl := client.New(path, 5*time.Second)
	doc, err := cl.Accounts(ctx, []string{"claude:work"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != model.SchemaVersion || len(doc.Accounts) != 1 || doc.Accounts[0].Verdict != model.Usable {
		t.Fatalf("doc %+v", doc)
	}
	if _, err := cl.Accounts(ctx, []string{"nope:x"}, false); err == nil {
		t.Fatal("unknown account accepted")
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("second listener on a live socket")
	}
	missing := client.New(filepath.Join(dir, "absent.sock"), time.Second)
	if _, err := missing.Health(ctx); !errors.Is(err, client.ErrDaemonDown) {
		t.Fatalf("absent socket: %v", err)
	}
}

func TestEnsurePrivateDirRefusesOpenModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	if err := EnsurePrivateDir(dir); err == nil {
		t.Fatal("0755 state dir accepted")
	}
}
