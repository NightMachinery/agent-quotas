// Package agy reads Antigravity (agy) quota per configured account: from the
// statusline cache agy's own hook keeps up to date, or, on a forced refresh
// or when the cache has nothing, from `agy -p /usage` (the slow path). It is
// the port of agy-status.zsh; docs/design.md has the contract.
package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
	"github.com/NightMachinery/agent-quotas/internal/render"
)

// Account is one configured Antigravity account with its paths resolved.
type Account struct {
	Name      string
	Label     string
	GeminiDir string // absolute
	CacheFile string // the statusline cache
	// Keychain: the account lives in the default ~/.gemini on macOS, so
	// agy's keychain sign-in is its own (see StoredAuth).
	Keychain bool
}

// CLIDir is the account's antigravity-cli directory.
func (a Account) CLIDir() string { return filepath.Join(a.GeminiDir, "antigravity-cli") }

// TokenFile is the account's file credential. Only its existence is ever
// looked at; its contents are never opened.
func (a Account) TokenFile() string { return filepath.Join(a.CLIDir(), "antigravity-oauth-token") }

// SignedIn reports whether the token file exists and is not empty (`test -s`).
func (a Account) SignedIn() bool {
	st, err := os.Stat(a.TokenFile())
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// keychainArgv asks whether agy's macOS keychain sign-in exists. Without -g
// or -w, security prints only the item's attributes, never the secret; it
// exits 44 when there is no such item.
var keychainArgv = []string{"/usr/bin/security", "find-generic-password", "-s", "gemini", "-a", "antigravity"}

// Where an account's sign-in is stored, as auth_status names it before a
// read proves the sign-in works ("verified").
const (
	AuthFile     = "file-present"
	AuthKeychain = "keychain"
	AuthMissing  = "missing"
	// AuthUnknown: no token file, and the keychain could not be asked (the
	// lookup timed out, as it does when the Mac swaps, or failed some other
	// way). It is not a sign-out, so a read goes on to the quota check.
	AuthUnknown = "unknown"
)

// keychainNotFound is security's exit status for "no such item"
// (errSecItemNotFound); any other failure leaves the sign-in unknown.
const keychainNotFound = 44

// StoredAuth says where a's sign-in is stored, without opening it: the token
// file, the macOS keychain, or nowhere. agy (1.5 and later) authenticates
// through a chain: the token file, then the keychain, even when told to
// store tokens in the file. A run without file mode keeps account m's token
// only in the keychain, and then m works with no token file at all. The
// keychain item is one per user, not per Gemini root, so it counts only for
// the account in the default root. `agent-quotas agy-auth` hands this answer
// to the shell, so the rule lives only here.
func StoredAuth(ctx context.Context, run procx.Runner, a Account) string {
	if a.SignedIn() {
		return AuthFile
	}
	if a.Keychain {
		res, err := run.Run(ctx, procx.Cmd{Argv: keychainArgv, Timeout: 5 * time.Second})
		switch {
		case err == nil && res.ExitCode == 0:
			return AuthKeychain
		case res.ExitCode != keychainNotFound:
			return AuthUnknown
		}
	}
	return AuthMissing
}

// main reports whether name is the account that keeps the original
// ~/.gemini state and the shared cache location.
func mainAccount(name string) bool { return name == "m" || name == "acc1" }

// ResolveAccount applies the defaults of h-agy-account-info: account m (or
// acc1) lives in ~/.gemini and keeps its cache at
// ~/tmp/.agy-status/quota-cache.json; any other account lives in
// ~/.agy-accounts/<name> with its cache under antigravity-cli/.
func ResolveAccount(c config.AgyAccount) Account {
	a := Account{Name: c.Name, Label: c.Label}
	if a.Label == "" {
		a.Label = c.Name
	}
	dir := config.Expand(c.GeminiDir)
	if dir == "" {
		if mainAccount(c.Name) {
			dir = filepath.Join(config.Home(), ".gemini")
		} else {
			dir = filepath.Join(config.Home(), ".agy-accounts", c.Name)
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	a.GeminiDir = dir
	if runtime.GOOS == "darwin" {
		def := filepath.Join(config.Home(), ".gemini")
		if real, err := filepath.EvalSymlinks(def); err == nil {
			def = real
		}
		a.Keychain = dir == def
	}
	switch {
	case c.CacheFile != "":
		a.CacheFile = config.Expand(c.CacheFile)
	case mainAccount(c.Name):
		a.CacheFile = filepath.Join(config.Home(), "tmp", ".agy-status", "quota-cache.json")
	default:
		a.CacheFile = filepath.Join(a.CLIDir(), "quota-cache.json")
	}
	return a
}

// Reader is the Antigravity model.Reader.
type Reader struct {
	cfg *config.Config
	run procx.Runner

	// Now is the clock when ReadOptions.Now is unset; nil means time.Now.
	Now func() time.Time
	// Location is the zone of resets_at_iso in the legacy rows; nil means
	// time.Local (gdate's --rfc-3339 prints the local zone).
	Location *time.Location
	// CacheOnly never runs agy: a missing or empty cache is an error, as
	// agy_status_cache_only_p=y made it.
	CacheOnly bool
	// StaleRefresh lets reads re-read an account whose reading is old or
	// predates a reset (see refreshIfStale). Only the daemon sets it: it
	// reads every account each minute anyway, while a command run by hand
	// should not stall on a slow-path run it did not ask for.
	StaleRefresh bool

	mu       sync.Mutex
	lastSlow map[string]time.Time
	// runs is each account's latest slow-path run, from start to end (zero
	// while it runs), for ownTouch.
	runs map[string][2]time.Time
}

// NewReader returns a reader over cfg.Agy.Accounts that runs agy with run.
func NewReader(cfg *config.Config, run procx.Runner) *Reader {
	return &Reader{cfg: cfg, run: run, lastSlow: map[string]time.Time{}, runs: map[string][2]time.Time{}}
}

// Provider is "agy".
func (r *Reader) Provider() string { return model.Agy }

// Accounts lists the configured accounts as agy:<name>.
func (r *Reader) Accounts(context.Context) ([]model.AccountRef, error) {
	var refs []model.AccountRef
	for _, c := range r.cfg.Agy.Accounts {
		a := ResolveAccount(c)
		refs = append(refs, model.AccountRef{
			ID: model.ID(model.Agy, a.Name), Provider: model.Agy, Name: a.Name,
			Extra: map[string]string{"gemini_dir": a.GeminiDir, "cache_file": a.CacheFile},
		})
	}
	return refs, nil
}

// CacheFiles maps each account name to its statusline cache, for the
// renderers' cache-age header (render.RenderAgy, render.Envelope).
func (r *Reader) CacheFiles() map[string]string { return CacheFiles(r.cfg) }

// CacheFiles maps each configured account name to its statusline cache.
func CacheFiles(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, c := range cfg.Agy.Accounts {
		a := ResolveAccount(c)
		out[a.Name] = a.CacheFile
	}
	return out
}

func (r *Reader) account(name string) (Account, bool) {
	for _, c := range r.cfg.Agy.Accounts {
		if c.Name == name {
			return ResolveAccount(c), true
		}
	}
	return Account{}, false
}

func (r *Reader) clock(opts model.ReadOptions) time.Time {
	if opts.Now != nil {
		return opts.Now()
	}
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reader) loc() *time.Location {
	if r.Location != nil {
		return r.Location
	}
	return time.Local
}

// LegacyBucket is one bucket of the agy-status --json account object.
type LegacyBucket struct {
	Group            string  `json:"group"`
	Label            string  `json:"label"`
	RemainingPercent float64 `json:"remaining_percent"`
	ResetsAt         float64 `json:"resets_at"`
	ResetsAtISO      string  `json:"resets_at_iso"`
	Source           string  `json:"source"` // cache or live
	BucketID         string  `json:"bucket_id,omitempty"`
	UsedPercent      float64 `json:"used_percent"`
}

// LegacyAccount is one account object of agy-status --json, the
// snapshot's Native field.
type LegacyAccount struct {
	Account    string          `json:"account"`
	Label      string          `json:"label"`
	SignedIn   *bool           `json:"signed_in"`
	AuthStatus string          `json:"auth_status"` // missing, file-present, keychain, verified
	Tier       json.RawMessage `json:"tier"`
	Email      json.RawMessage `json:"email"`
	Message    *string         `json:"message"`
	Error      *string         `json:"error"`
	Buckets    []LegacyBucket  `json:"buckets"`
}

// readResult is what one read produced, before it becomes a snapshot.
type readResult struct {
	rows     []LegacyBucket
	source   string // statusline or slow
	notes    []model.Note
	fetched  *float64
	err      error
	exit     int
	captures map[string]float64 // bucket id -> captured_at (cache rows)
}

// Read reads one account. Normal reads serve the statusline cache; a forced
// read (or a signed-in account whose cache has nothing) runs the slow path
// and merges its rows into the cache. When nothing usable came back, Read
// returns an error together with a snapshot whose Error and Native say why,
// so a caller with no earlier reading can still show the account.
func (r *Reader) Read(ctx context.Context, ref model.AccountRef, opts model.ReadOptions) (model.Snapshot, error) {
	a, ok := r.account(ref.Name)
	if !ok || (ref.Provider != "" && ref.Provider != model.Agy) {
		return model.Snapshot{}, fmt.Errorf("%w: %s", model.ErrUnknownAccount, ref.ID)
	}
	now := r.clock(opts)
	legacy := LegacyAccount{Account: a.Name, Label: a.Label, Tier: jsonNull, Email: jsonNull, Buckets: []LegacyBucket{}}
	meta := cacheMetadata(a.CacheFile)
	legacy.Tier = metaValue(meta, "tier")
	legacy.Email = metaValue(meta, "email")

	stored := "file credentials exist"
	switch legacy.AuthStatus = StoredAuth(ctx, r.run, a); legacy.AuthStatus {
	case AuthKeychain:
		stored = "a keychain sign-in exists"
	case AuthUnknown:
		stored = "the keychain could not be checked"
	case AuthMissing:
		legacy.SignedIn = model.B(false)
		legacy.Message = model.S("Not signed in; run agy-acc-login " + a.Name)
		return r.snapshot(a, legacy, readResult{source: "statusline"}, now), nil
	}
	res := r.quota(ctx, a, opts.Force, now)
	if res.err != nil {
		legacy.Error = model.S(fmt.Sprintf("Quota check failed (exit %d); %s, validity unverified", res.exit, stored))
		return r.snapshot(a, legacy, res, now), res.err
	}
	legacy.Buckets = res.rows
	if res.source == "slow" {
		legacy.SignedIn = model.B(true)
		legacy.AuthStatus = "verified"
	}
	return r.snapshot(a, legacy, res, now), nil
}

var jsonNull = json.RawMessage("null")

// metaValue is `$metadata.k // null`.
func metaValue(meta rawObject, k string) json.RawMessage {
	v, ok := meta.get(k)
	if !ok {
		return jsonNull
	}
	if t := strings.TrimSpace(string(v)); t == "null" || t == "false" {
		return jsonNull
	}
	return v
}

// quota is h-agy-status-statusline (and its fallback to h-agy-status-slow).
func (r *Reader) quota(ctx context.Context, a Account, force bool, now time.Time) readResult {
	if force {
		return r.slow(ctx, a, now, "agy (deus)")
	}
	data, err := os.ReadFile(a.CacheFile)
	if err != nil {
		return r.fallback(ctx, a, now, "agy (no cache; run `agy-statusline-install')")
	}
	maxAge := r.maxAge()
	doc, perr := parseRawObject(data)
	var rows []cacheRow
	if perr == nil {
		rows = cacheRows(doc, float64(now.Unix()), maxAge)
	}
	if len(rows) == 0 {
		if perr != nil && !errors.Is(perr, errNotObject) {
			return r.fallback(ctx, a, now, "agy (cache not valid JSON: "+a.CacheFile+")")
		}
		return r.fallback(ctx, a, now, "agy (cache older than "+ageFmt(maxAge)+")")
	}
	if res, ok := r.refreshIfUsed(ctx, a, rows, now); ok {
		return res
	}
	if res, ok := r.refreshIfStale(ctx, a, rows, now); ok {
		return res
	}
	res := readResult{source: "statusline", captures: map[string]float64{}}
	oldest, undated := int64(0), false
	var sources []string
	seen := map[string]bool{}
	var earliest *float64
	for _, row := range rows {
		res.rows = append(res.rows, r.cacheBucket(row))
		if row.Captured < 0 {
			undated = true
		} else {
			res.captures[row.ID] = row.Captured
			age := now.Unix() - int64(row.Captured)
			if age > oldest {
				oldest = age
			}
			if earliest == nil || row.Captured < *earliest {
				earliest = model.F(row.Captured)
			}
		}
		if !seen[row.Source] {
			seen[row.Source] = true
			sources = append(sources, row.Source)
		}
	}
	var descs []string
	for _, s := range sources {
		descs = append(descs, writerDesc(s))
	}
	detail := "via " + strings.Join(descs, ", ")
	if undated {
		detail += "; some rows undated"
	}
	res.notes = append(res.notes, model.Note{Severity: model.Info, Text: "Cache " + ageFmt(int(oldest)) + " old (" + detail + ")"})
	res.fetched = earliest
	return res
}

// maxAge is statusline_max_age_s: how old a cached bucket may be and still
// be served; below 0, any age.
func (r *Reader) maxAge() int {
	if r.cfg.Agy.StatuslineMaxAgeS != nil {
		return *r.cfg.Agy.StatuslineMaxAgeS
	}
	return -1
}

// cacheBucket is a cached bucket as a report row.
func (r *Reader) cacheBucket(row cacheRow) LegacyBucket {
	group, label := bucketDisplayNames(row.ID)
	if row.Group != "" && row.Label != "" {
		group, label = row.Group, row.Label
	}
	return LegacyBucket{
		Group: group, Label: label, RemainingPercent: row.Remaining, ResetsAt: row.ResetsAt,
		ResetsAtISO: rfc3339s(row.ResetsAt, r.loc()), Source: "cache", BucketID: row.ID,
		UsedPercent: 100 - row.Remaining,
	}
}

// refreshIfUsed runs the slow path when the account was used after the
// cache's newest capture. Only agy's TUI renders a statusline, so agy run
// any other way (`agy acp` under Paseo, `agy -p`) spends quota while the
// cache stands still; without this, the cache would be served as current
// for as long as it exists. The slow path rewrites the cache, so its new
// capture time is what spaces the runs out: at most one per
// active_refresh_s (default 300) per account, and lastSlow covers a run that
// failed. ok is false when the cache should be served as it is, which is
// also what a failed refresh does: an old reading beats an error.
func (r *Reader) refreshIfUsed(ctx context.Context, a Account, rows []cacheRow, now time.Time) (readResult, bool) {
	if r.CacheOnly || r.cfg.Agy.ActiveRefreshS < 0 {
		return readResult{}, false
	}
	newest := -1.0
	for _, row := range rows {
		newest = math.Max(newest, row.Captured)
	}
	if newest < 0 {
		return readResult{}, false
	}
	used := a.LastUsed()
	if r.ownTouch(used) {
		return readResult{}, false
	}
	s := r.cfg.Agy.ActiveRefreshS
	if s == 0 {
		s = 300 // a config built without config.Load's defaults
	}
	interval := time.Duration(s) * time.Second
	// Compared in whole seconds, as captures are stamped: see slow.
	if used.IsZero() || used.Unix() <= int64(newest) || now.Sub(time.Unix(int64(newest), 0)) < interval {
		return readResult{}, false
	}
	r.mu.Lock()
	last, ran := r.lastSlow[a.Name]
	r.mu.Unlock()
	if ran && now.Sub(last) < interval && now.Sub(last) >= 0 {
		return readResult{}, false
	}
	res := r.slow(ctx, a, now, "agy (used since the cache was written)")
	if res.err != nil {
		return readResult{}, false
	}
	return res, true
}

// refreshIfStale keeps the reading of an account nobody is using current.
// Without it, an idle account's cache, or one used only from another
// device, is served for days as if it were the account's quota. It runs the
// slow path (no quota spent) when the newest reading is older than
// stale_refresh_s (default an hour), or when a pool's reset has passed
// since that pool was read, blocked_grace_s after the reset so agy has
// rolled the window over. lastSlow spaces the runs: a failed run waits a
// full interval, and a passed reset triggers at most one run, even when the
// run does not print that pool again. ok is false when the cache should be
// served as it is, as after a failed run.
func (r *Reader) refreshIfStale(ctx context.Context, a Account, rows []cacheRow, now time.Time) (readResult, bool) {
	if !r.StaleRefresh || r.CacheOnly || r.cfg.Agy.StaleRefreshS < 0 {
		return readResult{}, false
	}
	s := r.cfg.Agy.StaleRefreshS
	if s == 0 {
		s = 3600 // a config built without config.Load's defaults
	}
	grace := r.cfg.Poll.BlockedGraceS
	if grace <= 0 {
		grace = 90
	}
	interval := time.Duration(s) * time.Second
	r.mu.Lock()
	last, ran := r.lastSlow[a.Name]
	r.mu.Unlock()
	nowS := float64(now.Unix())
	newest := -1.0
	why := ""
	for _, row := range rows {
		newest = math.Max(newest, row.Captured)
		reset := row.ResetsAt
		if why == "" && reset > row.Captured && reset+float64(grace) <= nowS && (!ran || float64(last.Unix()) < reset) {
			family, window := render.BucketNames(row.ID, row.Group, row.Label)
			why = "agy (" + strings.TrimSpace(family+" "+window) + " reset since it was read)"
		}
	}
	if why == "" {
		if newest >= 0 && now.Sub(time.Unix(int64(newest), 0)) < interval {
			return readResult{}, false
		}
		if ran && now.Sub(last) < interval && now.Sub(last) >= 0 {
			return readResult{}, false
		}
		why = "agy (reading older than " + ageFmt(s) + ")"
	}
	res := r.slow(ctx, a, now, why)
	if res.err != nil {
		return readResult{}, false
	}
	return res, true
}

// ownTouch reports whether t, an account's last use, fell during one of
// this reader's slow-path runs, of any account: `agy -p /usage` touches the
// conversations it runs against, and a second account's conversations may
// link to the main account's, so its run would otherwise read as use of
// the main account. Whole seconds, as file times and captures compare.
func (r *Reader) ownTouch(t time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.runs {
		start, end := run[0].Unix(), run[1].Unix()
		if run[1].IsZero() {
			end = math.MaxInt64 // still running
		}
		if t.Unix() >= start && t.Unix() <= end {
			return true
		}
	}
	return false
}

// LastUsed is when agy last wrote to this account's state: a conversation
// or its summary index, which every turn touches, TUI or not. Zero when
// neither exists. Lstat, not Stat: a second account may symlink its
// conversations to the main account's, and following the link would count
// the main account's turns as its own.
func (a Account) LastUsed() time.Time {
	var t time.Time
	for _, name := range []string{"conversations", "conversation_summaries.db"} {
		if fi, err := os.Lstat(filepath.Join(a.CLIDir(), name)); err == nil && fi.Mode()&os.ModeSymlink == 0 && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
	}
	return t
}

// fallback is the slow path taken because the cache had nothing to serve.
// It is said, never silent, and it runs at most once per slow_fallback_s
// (default an hour) per account: the cache stays empty until agy runs, and every attempt is a
// full CLI start-up.
func (r *Reader) fallback(ctx context.Context, a Account, now time.Time, why string) readResult {
	if r.CacheOnly {
		return readResult{exit: 1, notes: []model.Note{{Severity: model.Info, Text: why}},
			err: errors.New("no usable cached quota; cache-only mode skips agy/network")}
	}
	r.mu.Lock()
	last, ran := r.lastSlow[a.Name]
	r.mu.Unlock()
	if interval := time.Duration(r.cfg.Agy.SlowFallbackS) * time.Second; ran && now.Sub(last) < interval && now.Sub(last) >= 0 {
		return readResult{exit: 1, notes: []model.Note{{Severity: model.Info, Text: why}},
			err: fmt.Errorf("the cache has nothing to serve and the slow path already ran %s ago", ageFmt(int(now.Sub(last).Seconds())))}
	}
	return r.slow(ctx, a, now, why)
}

func (r *Reader) slow(ctx context.Context, a Account, now time.Time, why string) readResult {
	if r.CacheOnly { // a forced read (deus) comes here directly
		return readResult{exit: 1, notes: []model.Note{{Severity: model.Info, Text: why}},
			err: errors.New("no usable cached quota; cache-only mode skips agy/network")}
	}
	r.mu.Lock()
	r.lastSlow[a.Name] = now
	r.runs[a.Name] = [2]time.Time{now}
	r.mu.Unlock()
	note := model.Note{Severity: model.Info, Text: why}
	start := time.Now()
	rows, err := r.runUsage(ctx, a)
	r.mu.Lock()
	r.runs[a.Name] = [2]time.Time{now, now.Add(time.Since(start))}
	r.mu.Unlock()
	if err != nil {
		var se *slowError
		exit := 1
		if errors.As(err, &se) {
			exit = se.exit
		}
		return readResult{source: "slow", notes: []model.Note{note}, err: err, exit: exit}
	}
	// The reading is stamped when the run ends, not when it starts: agy
	// touches the account's conversations while it runs, and a capture
	// older than that touch would read as use since the cache was written,
	// so refreshIfUsed would start the slow path again every
	// active_refresh_s, for ever.
	done := now.Add(time.Since(start))
	res := readResult{source: "slow", notes: []model.Note{note}, fetched: model.F(float64(done.Unix())), captures: map[string]float64{}}
	printed := map[string]bool{}
	for _, row := range rows {
		res.rows = append(res.rows, LegacyBucket{
			Group: row.Group, Label: row.Label, RemainingPercent: row.Remaining, ResetsAt: float64(row.ResetsAt),
			ResetsAtISO: row.ISO, Source: "live", BucketID: row.BucketID, UsedPercent: 100 - row.Remaining,
		})
		printed[row.BucketID] = true
	}
	// agy lists a model's own pool (Gemini 4 Argon) only some of the time.
	// A pool this run did not print stays in the report as the cache has
	// it, as a cache read would show it, instead of vanishing until the
	// next one.
	if data, err := os.ReadFile(a.CacheFile); err == nil {
		if doc, err := parseRawObject(data); err == nil {
			for _, c := range cacheRows(doc, float64(now.Unix()), r.maxAge()) {
				if printed[c.ID] {
					continue
				}
				res.rows = append(res.rows, r.cacheBucket(c))
				if c.Captured >= 0 {
					res.captures[c.ID] = c.Captured
				}
			}
		}
	}
	// The slow path is the authority on the weekly pools, so the cache
	// keeps what it learned. Best effort: a cache we could not update must
	// not cost the reading we already have.
	if err := mergeSlow(a.CacheFile, rows, done.Unix()); err != nil {
		res.notes = append(res.notes, model.Note{Severity: model.Warning, Text: "could not update the statusline cache: " + err.Error()})
	}
	return res
}

// writerDesc is h-agy-status-writer-desc: what a cache writer is.
func writerDesc(source string) string {
	switch source {
	case "slow":
		return "`agy -p /usage'"
	case "statusline":
		return "agy's statusline hook"
	case "":
		return "unknown"
	}
	return source
}

// ageFmt is h-agy-status-age-fmt: one unit of precision.
func ageFmt(s int) string {
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dd", s/86400)
}

// rfc3339s is `gdate -d @epoch --rfc-3339=s` in loc.
func rfc3339s(epoch float64, loc *time.Location) string {
	return time.Unix(int64(math.Floor(epoch)), 0).In(loc).Format("2006-01-02 15:04:05-07:00")
}

func generalFamily(bucketID string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(bucketID, "-5h"), "-weekly")
	if base == "gemini" || base == "3p" {
		return base
	}
	return ""
}

// snapshot turns a legacy row into the normalized snapshot.
func (r *Reader) snapshot(a Account, legacy LegacyAccount, res readResult, now time.Time) model.Snapshot {
	native, _ := marshal(legacy)
	s := model.Snapshot{
		ID: model.ID(model.Agy, a.Name), Provider: model.Agy, Name: a.Name, Label: a.Label,
		BlockedReasons: []string{}, Windows: []model.Window{}, Notes: append([]model.Note{}, res.notes...),
		FetchedAt: res.fetched, Source: res.source, Native: native,
	}
	var tier, email string
	if json.Unmarshal(legacy.Tier, &tier) == nil && tier != "" {
		s.Plan = model.S(tier)
	}
	if json.Unmarshal(legacy.Email, &email) == nil && email != "" {
		s.Email = model.S(email)
	}
	if legacy.SignedIn != nil && !*legacy.SignedIn {
		s.Verdict = model.SignedOut
		if legacy.Message != nil {
			s.Notes = append(s.Notes, model.Note{Severity: model.Info, Text: *legacy.Message})
		}
		return s
	}
	if res.err != nil {
		s.Verdict = model.Unknown
		s.Error = model.S(res.err.Error())
		return s
	}

	nowS := float64(now.Unix())
	type familyState struct {
		blocked bool
		latest  float64
		reasons []string
	}
	families := map[string]*familyState{}
	var order []string
	var expired []string
	for _, b := range legacy.Buckets {
		family, window := render.BucketNames(b.BucketID, b.Group, b.Label)
		w := model.Window{
			Key: b.BucketID, Family: family, Window: window, Label: strings.TrimSpace(b.Group + ": " + b.Label),
			UsedPercent: model.F(b.UsedPercent), Scope: model.ScopeModel, Blocking: b.RemainingPercent <= 0,
		}
		if b.ResetsAt > 0 && finite(b.ResetsAt) {
			w.ResetsAt = model.F(b.ResetsAt)
		}
		switch window {
		case "5h":
			w.WindowMinutes = model.F(300)
		case "weekly":
			w.WindowMinutes = model.F(10080)
		}
		// A reading taken before its window reset says nothing about the
		// window now: it no longer blocks (the old armed job trusted the
		// reset time the same way), and the snapshot says it is stale.
		if w.Blocking && w.ResetsAt != nil && *w.ResetsAt <= nowS {
			captured, ok := res.captures[b.BucketID] // live rows have none
			if !ok || captured < *w.ResetsAt {
				w.Blocking = false
				expired = append(expired, family+" "+window)
			}
		}
		if g := generalFamily(b.BucketID); g != "" {
			w.Scope = model.ScopeAccount
			fs := families[g]
			if fs == nil {
				fs = &familyState{}
				families[g] = fs
				order = append(order, g)
			}
			if w.Blocking {
				fs.blocked = true
				fs.reasons = append(fs.reasons, family+" "+window+" at 100%")
				if w.ResetsAt != nil && *w.ResetsAt > fs.latest {
					fs.latest = *w.ResetsAt
				}
			}
		}
		s.Windows = append(s.Windows, w)
	}
	if len(expired) > 0 {
		s.Stale = true
		s.StaleReason = model.S("reading predates the reset of " + strings.Join(expired, ", "))
	}
	switch {
	case len(families) == 0:
		s.Verdict = model.Unknown
		s.Notes = append(s.Notes, model.Note{Severity: model.Warning, Text: "no general (Gemini or Claude/GPT) pool reported"})
	default:
		all := true
		for _, g := range order {
			if !families[g].blocked {
				all = false
			}
		}
		if !all {
			s.Verdict = model.Usable
			break
		}
		s.Verdict = model.Blocked
		// The first family back is usable again; within a family, every
		// blocking window has to reset.
		var until float64
		for _, g := range order {
			fs := families[g]
			s.BlockedReasons = append(s.BlockedReasons, fs.reasons...)
			if fs.latest > 0 && (until == 0 || fs.latest < until) {
				until = fs.latest
			}
		}
		if until > 0 {
			s.BlockedUntil = model.F(until)
		}
	}
	return s
}
