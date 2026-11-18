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

	mu       sync.Mutex
	lastSlow map[string]time.Time
}

// NewReader returns a reader over cfg.Agy.Accounts that runs agy with run.
func NewReader(cfg *config.Config, run procx.Runner) *Reader {
	return &Reader{cfg: cfg, run: run, lastSlow: map[string]time.Time{}}
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
	AuthStatus string          `json:"auth_status"` // missing, file-present, verified
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

	if !a.SignedIn() {
		legacy.AuthStatus = "missing"
		legacy.SignedIn = model.B(false)
		legacy.Message = model.S("Not signed in; run agy-acc-login " + a.Name)
		return r.snapshot(a, legacy, readResult{source: "statusline"}, now), nil
	}
	legacy.AuthStatus = "file-present"
	res := r.quota(ctx, a, opts.Force, now)
	if res.err != nil {
		legacy.Error = model.S(fmt.Sprintf("Quota check failed (exit %d); file credentials exist, validity unverified", res.exit))
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
	maxAge := -1
	if r.cfg.Agy.StatuslineMaxAgeS != nil {
		maxAge = *r.cfg.Agy.StatuslineMaxAgeS
	}
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
	res := readResult{source: "statusline", captures: map[string]float64{}}
	oldest, undated := int64(0), false
	var sources []string
	seen := map[string]bool{}
	var earliest *float64
	for _, row := range rows {
		group, label := bucketDisplayNames(row.ID)
		if row.Group != "" && row.Label != "" {
			group, label = row.Group, row.Label
		}
		res.rows = append(res.rows, LegacyBucket{
			Group: group, Label: label, RemainingPercent: row.Remaining, ResetsAt: row.ResetsAt,
			ResetsAtISO: rfc3339s(row.ResetsAt, r.loc()), Source: "cache", BucketID: row.ID,
			UsedPercent: 100 - row.Remaining,
		})
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

// fallback is the slow path taken because the cache had nothing to serve.
// It is said, never silent, and it runs at most once per min_interval per
// account: the cache stays empty until agy runs, and every attempt is a
// full CLI start-up.
func (r *Reader) fallback(ctx context.Context, a Account, now time.Time, why string) readResult {
	if r.CacheOnly {
		return readResult{exit: 1, notes: []model.Note{{Severity: model.Info, Text: why}},
			err: errors.New("no usable cached quota; cache-only mode skips agy/network")}
	}
	r.mu.Lock()
	last, ran := r.lastSlow[a.Name]
	r.mu.Unlock()
	if interval := time.Duration(r.cfg.Agy.MinIntervalS) * time.Second; ran && now.Sub(last) < interval && now.Sub(last) >= 0 {
		return readResult{exit: 1, notes: []model.Note{{Severity: model.Info, Text: why}},
			err: fmt.Errorf("the cache has nothing to serve and the slow path already ran %s ago", ageFmt(int(now.Sub(last).Seconds())))}
	}
	return r.slow(ctx, a, now, why)
}

func (r *Reader) slow(ctx context.Context, a Account, now time.Time, why string) readResult {
	r.mu.Lock()
	r.lastSlow[a.Name] = now
	r.mu.Unlock()
	note := model.Note{Severity: model.Info, Text: why}
	rows, err := r.runUsage(ctx, a)
	if err != nil {
		var se *slowError
		exit := 1
		if errors.As(err, &se) {
			exit = se.exit
		}
		return readResult{source: "slow", notes: []model.Note{note}, err: err, exit: exit}
	}
	res := readResult{source: "slow", notes: []model.Note{note}, fetched: model.F(float64(now.Unix()))}
	for _, row := range rows {
		res.rows = append(res.rows, LegacyBucket{
			Group: row.Group, Label: row.Label, RemainingPercent: row.Remaining, ResetsAt: float64(row.ResetsAt),
			ResetsAtISO: row.ISO, Source: "live", BucketID: row.BucketID, UsedPercent: 100 - row.Remaining,
		})
	}
	// The slow path is the authority on the weekly pools, so the cache
	// keeps what it learned. Best effort: a cache we could not update must
	// not cost the reading we already have.
	if err := mergeSlow(a.CacheFile, rows, now.Unix()); err != nil {
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
			captured, ok := res.captures[b.BucketID]
			if res.source == "slow" || !ok || captured < *w.ResetsAt {
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
