// Package daemon owns every account's latest snapshot: one poller per
// account, a cache, forced-refresh coalescing with a floor, back-off on rate
// limits, verdict transitions, and the unix-socket API (server.go).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
)

// TransitionFunc is called after an account's verdict changed. It runs on
// its own goroutine and must not block the poller for long.
type TransitionFunc func(t model.Transition, s model.Snapshot)

// Options tune a Daemon; zero values take the defaults.
type Options struct {
	StateDir string
	Now      func() time.Time
	Logger   *log.Logger
	// ReadTimeout bounds one upstream read (default 2 minutes).
	ReadTimeout time.Duration
	// DiscoverEvery is how often account lists are re-read (default 5 minutes).
	DiscoverEvery time.Duration
	// NoPersist disables snapshots.json and transitions.jsonl (tests).
	NoPersist bool
}

// Daemon is the store and its pollers.
type Daemon struct {
	cfg     *config.Config
	readers map[string]model.Reader
	opts    Options
	log     *log.Logger

	mu       sync.Mutex
	accounts map[string]*account
	order    []string
	subs     []TransitionFunc
	started  time.Time
	saved    map[string]model.Snapshot // readings from the previous run, by id

	persistMu sync.Mutex
	runCtx    context.Context
	wg        sync.WaitGroup
}

type call struct {
	done chan struct{}
	snap model.Snapshot
	err  error
}

type account struct {
	ref    model.AccountRef
	reader model.Reader

	mu           sync.Mutex
	snap         model.Snapshot
	hasData      bool // snap holds a reading (possibly stale)
	inflight     *call
	lastAttempt  time.Time
	lastSuccess  time.Time
	lastResult   *call // the latest finished call, for the floor
	backoffUntil time.Time
	failures     int
	upstream     int
	lastStatus   int

	wake   chan struct{}
	cancel context.CancelFunc
}

// New builds a Daemon over the given readers.
func New(cfg *config.Config, readers []model.Reader, opts Options) *Daemon {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "agent-quotas: ", log.LstdFlags)
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 2 * time.Minute
	}
	if opts.DiscoverEvery == 0 {
		opts.DiscoverEvery = 5 * time.Minute
	}
	if opts.StateDir == "" {
		opts.StateDir = config.StateDir()
	}
	d := &Daemon{cfg: cfg, readers: map[string]model.Reader{}, opts: opts, log: opts.Logger,
		accounts: map[string]*account{}, started: opts.Now()}
	for _, r := range readers {
		d.readers[r.Provider()] = r
	}
	return d
}

// Config returns the configuration the daemon runs with.
func (d *Daemon) Config() *config.Config { return d.cfg }

// Subscribe registers fn for verdict transitions.
func (d *Daemon) Subscribe(fn TransitionFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subs = append(d.subs, fn)
}

func (d *Daemon) now() time.Time { return d.opts.Now() }

// Discover (re)reads every reader's account list, adding new accounts and
// dropping those that disappeared. Pollers start for new accounts when the
// daemon is running.
func (d *Daemon) Discover(ctx context.Context) error {
	var errs []string
	seen := map[string]bool{}
	var found []*account
	providers := make([]string, 0, len(d.readers))
	for p := range d.readers {
		providers = append(providers, p)
	}
	sort.Slice(providers, func(i, j int) bool { return providerRank(providers[i]) < providerRank(providers[j]) })
	for _, p := range providers {
		r := d.readers[p]
		refs, err := r.Accounts(ctx)
		if err != nil {
			errs = append(errs, p+": "+err.Error())
			// Keep this provider's existing accounts rather than dropping them.
			d.mu.Lock()
			for _, id := range d.order {
				if a := d.accounts[id]; a != nil && a.ref.Provider == p {
					seen[id] = true
				}
			}
			d.mu.Unlock()
			continue
		}
		for _, ref := range refs {
			seen[ref.ID] = true
			found = append(found, &account{ref: ref, reader: r})
		}
	}
	d.mu.Lock()
	var order []string
	var added []*account
	for _, a := range found {
		if old, ok := d.accounts[a.ref.ID]; ok {
			old.mu.Lock()
			old.ref = a.ref
			old.mu.Unlock()
		} else {
			a.wake = make(chan struct{}, 1)
			a.snap = emptySnapshot(a.ref)
			if s, ok := d.saved[a.ref.ID]; ok {
				// Served at once after a restart, with its original age; the
				// poller reads again when that age reaches the interval.
				a.snap, a.hasData = s, true
			}
			d.accounts[a.ref.ID] = a
			added = append(added, a)
		}
	}
	for _, id := range d.order {
		if seen[id] {
			order = append(order, id)
		}
	}
	for _, a := range found {
		if !contains(order, a.ref.ID) {
			order = append(order, a.ref.ID)
		}
	}
	for id, a := range d.accounts {
		if !seen[id] {
			if a.cancel != nil {
				a.cancel()
			}
			delete(d.accounts, id)
		}
	}
	d.order = order
	runCtx := d.runCtx
	d.mu.Unlock()
	if runCtx != nil {
		for i, a := range added {
			d.startPoller(runCtx, a, time.Duration(i)*2*time.Second)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func providerRank(p string) int {
	switch p {
	case model.Claude:
		return 0
	case model.Codex:
		return 1
	case model.Agy:
		return 2
	}
	return 3
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func emptySnapshot(ref model.AccountRef) model.Snapshot {
	return model.Snapshot{ID: ref.ID, Provider: ref.Provider, Name: ref.Name, Label: ref.Name,
		Verdict: model.Unknown, BlockedReasons: []string{}, Windows: []model.Window{}, Notes: []model.Note{}}
}

// Run loads the saved snapshots, discovers accounts and runs the pollers
// until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	d.loadSnapshots()
	d.mu.Lock()
	d.runCtx = ctx
	d.mu.Unlock()
	if err := d.Discover(ctx); err != nil {
		d.log.Printf("discover: %v", err)
	}
	d.mu.Lock()
	accts := make([]*account, 0, len(d.order))
	for _, id := range d.order {
		accts = append(accts, d.accounts[id])
	}
	d.mu.Unlock()
	for i, a := range accts {
		d.startPoller(ctx, a, time.Duration(i)*2*time.Second)
	}
	ticker := time.NewTicker(d.opts.DiscoverEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return nil
		case <-ticker.C:
			if err := d.Discover(ctx); err != nil {
				d.log.Printf("discover: %v", err)
			}
		}
	}
}

func (d *Daemon) startPoller(ctx context.Context, a *account, stagger time.Duration) {
	pctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()
		cancel()
		return
	}
	a.cancel = cancel
	a.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.poll(pctx, a, stagger)
	}()
}

func (d *Daemon) minInterval(provider string) time.Duration {
	s := 300
	switch provider {
	case model.Claude:
		s = d.cfg.Claude.MinIntervalS
	case model.Codex:
		s = d.cfg.Codex.MinIntervalS
	case model.Agy:
		s = d.cfg.Agy.MinIntervalS
	}
	if s < 5 {
		s = 5
	}
	return time.Duration(s) * time.Second
}

// nextPoll is when the scheduler should read this account again.
func (d *Daemon) nextPoll(a *account) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return d.nextPollLocked(a)
}

func (d *Daemon) nextPollLocked(a *account) time.Time {
	interval := d.minInterval(a.ref.Provider)
	var next time.Time
	if a.lastAttempt.IsZero() {
		// Never attempted in this process: honour a saved reading's age.
		if a.hasData && a.snap.FetchedAt != nil && !a.snap.Stale {
			next = model.Time(*a.snap.FetchedAt).Add(interval)
		} else {
			next = d.now()
		}
	} else {
		next = a.lastAttempt.Add(interval)
	}
	if a.hasData && a.snap.Verdict == model.Blocked && a.snap.BlockedUntil != nil {
		reset := model.Time(*a.snap.BlockedUntil).Add(time.Duration(d.cfg.Poll.BlockedGraceS) * time.Second)
		floor := a.lastAttempt.Add(time.Duration(d.cfg.Poll.ForceFloorS) * time.Second)
		if reset.Before(floor) {
			reset = floor
		}
		if reset.Before(next) {
			next = reset
		}
	}
	if a.backoffUntil.After(next) {
		next = a.backoffUntil
	}
	return next
}

func (d *Daemon) poll(ctx context.Context, a *account, stagger time.Duration) {
	if stagger > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(stagger):
		}
	}
	for {
		next := d.nextPoll(a)
		wait := next.Sub(d.now())
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-a.wake:
				timer.Stop()
				continue // re-evaluate: a forced read may have changed the schedule
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		_, _ = d.refresh(ctx, a, false)
	}
}

// Wake makes the account's poller re-evaluate its schedule now.
func (a *account) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// ErrBackedOff is returned by a forced refresh refused because of back-off.
var ErrBackedOff = errors.New("backed off after upstream errors")

// refresh reads the account upstream, sharing an in-flight call. A forced
// refresh within the floor returns the latest result instead of calling.
func (d *Daemon) refresh(ctx context.Context, a *account, force bool) (model.Snapshot, error) {
	a.mu.Lock()
	if c := a.inflight; c != nil {
		a.mu.Unlock()
		return waitCall(ctx, c)
	}
	now := d.now()
	if force {
		floor := time.Duration(d.cfg.Poll.ForceFloorS) * time.Second
		if c := a.lastResult; c != nil && now.Sub(a.lastAttempt) < floor {
			a.mu.Unlock()
			return c.snap, c.err
		}
	}
	if now.Before(a.backoffUntil) {
		snap := d.decorate(a)
		until := a.backoffUntil
		a.mu.Unlock()
		return snap, fmt.Errorf("%w until %s", ErrBackedOff, until.Format(time.RFC3339))
	}
	c := &call{done: make(chan struct{})}
	a.inflight = c
	a.lastAttempt = now
	a.upstream++
	ref := a.ref
	reader := a.reader
	a.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.opts.ReadTimeout)
	snap, err := reader.Read(rctx, ref, model.ReadOptions{Force: force, Now: d.opts.Now})
	cancel()

	trans, out := d.apply(a, snap, err)
	c.snap, c.err = out, err
	a.mu.Lock()
	a.inflight = nil
	a.lastResult = c
	a.mu.Unlock()
	close(c.done)
	a.poke()
	if trans != nil {
		d.recordTransition(*trans, out)
	}
	d.persist()
	return out, err
}

func waitCall(ctx context.Context, c *call) (model.Snapshot, error) {
	select {
	case <-c.done:
		return c.snap, c.err
	case <-ctx.Done():
		return model.Snapshot{}, ctx.Err()
	}
}

// apply folds a read result into the account and returns the transition, if
// any, and the snapshot as readers will see it.
func (d *Daemon) apply(a *account, snap model.Snapshot, err error) (*model.Transition, model.Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := d.now()
	before := a.snap.Verdict
	hadData := a.hasData
	var up *model.UpstreamError
	if errors.As(err, &up) {
		a.lastStatus = up.Status
	} else if err == nil {
		a.lastStatus = 0
	}
	switch {
	case err == nil:
		fixIdentity(&snap, a.ref)
		a.snap = snap
		a.hasData = true
		a.failures = 0
		a.backoffUntil = time.Time{}
		a.lastSuccess = now
	default:
		a.failures++
		if retryable(err) {
			a.backoffUntil = now.Add(d.backoff(a.failures, up))
		}
		msg := err.Error()
		fallback := snap.Source != ""
		older := hadData && a.snap.FetchedAt != nil && (snap.FetchedAt == nil || *snap.FetchedAt <= *a.snap.FetchedAt)
		switch {
		case fallback && !older:
			fixIdentity(&snap, a.ref)
			if snap.Error == nil {
				snap.Error = model.S(msg)
			}
			snap.Stale = true
			if snap.StaleReason == nil {
				snap.StaleReason = model.S(msg)
			}
			a.snap = snap
			a.hasData = true
		case hadData:
			a.snap.Stale = true
			a.snap.StaleReason = model.S(msg)
			a.snap.Error = model.S(msg)
		default:
			s := emptySnapshot(a.ref)
			if snap.Verdict == model.SignedOut {
				s.Verdict = model.SignedOut
			}
			s.Error = model.S(msg)
			if len(snap.Native) > 0 {
				s.Native = snap.Native
			}
			a.snap = s
		}
	}
	after := a.snap.Verdict
	var t *model.Transition
	if before != after {
		t = &model.Transition{At: model.Unix(now), ID: a.ref.ID, From: before, To: after,
			Reasons: a.snap.BlockedReasons, ResetsAt: a.snap.BlockedUntil}
	}
	return t, d.decorate(a)
}

func fixIdentity(s *model.Snapshot, ref model.AccountRef) {
	s.ID, s.Provider, s.Name = ref.ID, ref.Provider, ref.Name
	if s.Label == "" {
		s.Label = ref.Name
	}
	if s.Verdict == "" {
		s.Verdict = model.Unknown
	}
	if s.BlockedReasons == nil {
		s.BlockedReasons = []string{}
	}
	if s.Windows == nil {
		s.Windows = []model.Window{}
	}
	if s.Notes == nil {
		s.Notes = []model.Note{}
	}
}

func retryable(err error) bool {
	var up *model.UpstreamError
	if errors.As(err, &up) {
		return up.Status == 0 || up.Status == 429 || up.Status >= 500
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func (d *Daemon) backoff(failures int, up *model.UpstreamError) time.Duration {
	base := 60 * time.Second * time.Duration(math.Pow(2, float64(failures-1)))
	if up != nil && up.RetryAfter > base {
		base = up.RetryAfter
	}
	max := time.Duration(d.cfg.Poll.BackoffMaxS) * time.Second
	if base > max || base <= 0 {
		base = max
	}
	return base
}

// decorate returns the account's snapshot with the poll bookkeeping filled
// in. Callers hold a.mu.
func (d *Daemon) decorate(a *account) model.Snapshot {
	s := a.snap
	p := model.PollInfo{ConsecutiveFailures: a.failures, UpstreamCalls: a.upstream, LastHTTPStatus: a.lastStatus}
	if !a.lastAttempt.IsZero() {
		p.LastAttemptAt = model.F(model.Unix(a.lastAttempt))
	}
	if !a.lastSuccess.IsZero() {
		p.LastSuccessAt = model.F(model.Unix(a.lastSuccess))
	}
	if !a.backoffUntil.IsZero() && a.backoffUntil.After(d.now()) {
		p.BackoffUntil = model.F(model.Unix(a.backoffUntil))
	}
	p.NextPollAt = model.F(model.Unix(d.nextPollLocked(a)))
	s.Poll = p
	s.Windows = append([]model.Window(nil), s.Windows...)
	return s
}

func (d *Daemon) recordTransition(t model.Transition, s model.Snapshot) {
	d.log.Printf("transition %s: %s -> %s", t.ID, t.From, t.To)
	if !d.opts.NoPersist {
		d.appendTransition(t)
	}
	d.mu.Lock()
	subs := append([]TransitionFunc(nil), d.subs...)
	d.mu.Unlock()
	for _, fn := range subs {
		go fn(t, s)
	}
}

// IDs returns the known account ids in display order.
func (d *Daemon) IDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.order...)
}

// Resolve expands selectors (an account id, a provider name, or empty for
// all) into account ids.
func (d *Daemon) Resolve(selectors []string) ([]string, error) {
	ids := d.IDs()
	if len(selectors) == 0 {
		return ids, nil
	}
	var out []string
	for _, sel := range selectors {
		matched := false
		for _, id := range ids {
			p, _, _ := model.SplitID(id)
			if id == sel || p == sel {
				if !contains(out, id) {
					out = append(out, id)
				}
				matched = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("%w: %s", model.ErrUnknownAccount, sel)
		}
	}
	return out, nil
}

// Get returns one account's snapshot, refreshing it first when force is set.
func (d *Daemon) Get(ctx context.Context, id string, force bool) (model.Snapshot, error) {
	d.mu.Lock()
	a := d.accounts[id]
	d.mu.Unlock()
	if a == nil {
		return model.Snapshot{}, fmt.Errorf("%w: %s", model.ErrUnknownAccount, id)
	}
	if force {
		snap, err := d.refresh(ctx, a, true)
		if errors.Is(err, ErrBackedOff) || errors.Is(err, context.Canceled) {
			return snap, err
		}
		// A failed forced read still answers with the best snapshot.
		a.mu.Lock()
		defer a.mu.Unlock()
		return d.decorate(a), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return d.decorate(a), nil
}

// GetMany returns the snapshots of ids, refreshing them concurrently when
// force is set. Errors of individual forced reads are reported inside each
// snapshot, not as a failure of the whole call.
func (d *Daemon) GetMany(ctx context.Context, ids []string, force bool) ([]model.Snapshot, error) {
	out := make([]model.Snapshot, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			s, err := d.Get(ctx, id, force)
			if errors.Is(err, ErrBackedOff) {
				if s.Error == nil {
					s.Error = model.S(err.Error())
				}
				err = nil
			}
			out[i], errs[i] = s, err
		}(i, id)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MaxAge refreshes id first when its reading is older than age (or absent).
// Used by the auto-continue engines before they act on a verdict.
func (d *Daemon) MaxAge(ctx context.Context, id string, age time.Duration) (model.Snapshot, error) {
	d.mu.Lock()
	a := d.accounts[id]
	d.mu.Unlock()
	if a == nil {
		return model.Snapshot{}, fmt.Errorf("%w: %s", model.ErrUnknownAccount, id)
	}
	a.mu.Lock()
	fresh := a.hasData && !a.lastSuccess.IsZero() && d.now().Sub(a.lastSuccess) < age && !a.snap.Stale
	a.mu.Unlock()
	return d.Get(ctx, id, !fresh)
}

// Kick wakes an account's poller and refreshes it (subject to the floor).
func (d *Daemon) Kick(ctx context.Context, id string) (model.Snapshot, error) {
	return d.Get(ctx, id, true)
}

// Health describes the running daemon.
type Health struct {
	SchemaVersion int     `json:"schema_version"`
	PID           int     `json:"pid"`
	StartedAt     float64 `json:"started_at"`
	Version       string  `json:"version"`
	Accounts      int     `json:"accounts"`
}

// Version is set by the CLI at start-up.
var Version = "dev"

// Health returns the daemon's health document.
func (d *Daemon) Health() Health {
	return Health{SchemaVersion: model.SchemaVersion, PID: os.Getpid(), StartedAt: model.Unix(d.started),
		Version: Version, Accounts: len(d.IDs())}
}

// ---- persistence ----

type savedSnapshots struct {
	SchemaVersion int              `json:"schema_version"`
	SavedAt       float64          `json:"saved_at"`
	Accounts      []model.Snapshot `json:"accounts"`
}

func (d *Daemon) snapshotsPath() string   { return filepath.Join(d.opts.StateDir, "snapshots.json") }
func (d *Daemon) transitionsPath() string { return filepath.Join(d.opts.StateDir, "transitions.jsonl") }

// loadSnapshots seeds accounts with the readings saved by a previous run,
// marked stale until a poll succeeds. Accounts are created lazily by
// Discover; saved readings wait in a side table until then.
func (d *Daemon) loadSnapshots() {
	if d.opts.NoPersist {
		return
	}
	data, err := os.ReadFile(d.snapshotsPath())
	if err != nil {
		return
	}
	var saved savedSnapshots
	if json.Unmarshal(data, &saved) != nil || saved.SchemaVersion != model.SchemaVersion {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.saved == nil {
		d.saved = map[string]model.Snapshot{}
	}
	for _, s := range saved.Accounts {
		s.Source = "snapshot-file"
		d.saved[s.ID] = s
	}
}

func (d *Daemon) persist() {
	if d.opts.NoPersist {
		return
	}
	d.persistMu.Lock()
	defer d.persistMu.Unlock()
	ids := d.IDs()
	doc := savedSnapshots{SchemaVersion: model.SchemaVersion, SavedAt: model.Unix(d.now())}
	for _, id := range ids {
		d.mu.Lock()
		a := d.accounts[id]
		d.mu.Unlock()
		if a == nil {
			continue
		}
		a.mu.Lock()
		if a.hasData {
			doc.Accounts = append(doc.Accounts, a.snap)
		}
		a.mu.Unlock()
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return
	}
	if err := WriteFileAtomic(d.snapshotsPath(), data, 0o600); err != nil {
		d.log.Printf("persist: %v", err)
	}
}

func (d *Daemon) appendTransition(t model.Transition) {
	d.persistMu.Lock()
	defer d.persistMu.Unlock()
	path := d.transitionsPath()
	if info, err := os.Stat(path); err == nil && info.Size() > 4<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		d.log.Printf("transitions: %v", err)
		return
	}
	defer f.Close()
	line, _ := json.Marshal(t)
	_, _ = f.Write(append(line, '\n'))
}

// Transitions returns recorded transitions at or after since.
func (d *Daemon) Transitions(since float64) ([]model.Transition, error) {
	out := []model.Transition{}
	for _, path := range []string{d.transitionsPath() + ".1", d.transitionsPath()} {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			var t model.Transition
			if line == "" || json.Unmarshal([]byte(line), &t) != nil {
				continue
			}
			if t.At >= since {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// WriteFileAtomic writes through a temp file, fsync and rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
