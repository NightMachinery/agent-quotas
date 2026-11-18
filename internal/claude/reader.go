// Package claude reads Claude Code subscription usage: it finds a profile's
// OAuth token, asks the usage endpoint Claude Code itself uses, and
// normalizes the answer into a model.Snapshot. It is a port of
// claude_code_usage.py from the scripts repository, minus its response cache
// (the daemon caches) and its relogin (no `claude -p /usage` is ever run).
//
// The snapshot's Native field is the legacy per-profile JSON row of that
// tool, so `agent-quotas claude-code-usage` can print the old outputs from a
// snapshot without reading upstream again.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// Reader is the Claude usage reader. Its fields after NewReader may be
// replaced in tests.
type Reader struct {
	cfg *config.Config
	run procx.Runner
	hc  *http.Client

	// URL is the usage endpoint.
	URL string
	// Now is the clock used when ReadOptions.Now is unset.
	Now func() time.Time
	// Getenv looks up an environment variable.
	Getenv func(string) (string, bool)
	// Home is the home directory (Keychains, ~/.claude.json, ~ expansion).
	Home string
	// GOOS decides whether the Keychain is consulted ("darwin" only).
	GOOS string
	// PasswdName is this uid's account name from the passwd database.
	PasswdName func() string
	// Location is the zone of the ISO timestamps in the legacy row.
	Location *time.Location

	// set per read
	home            string
	goos            string
	timeout         time.Duration
	keychainTimeout time.Duration
	clockFn         func() time.Time
}

// NewReader returns a reader for cfg's Claude profiles. A nil runner or
// client means the real ones.
func NewReader(cfg *config.Config, run procx.Runner, hc *http.Client) *Reader {
	if run == nil {
		run = procx.Exec{}
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Reader{
		cfg:      cfg,
		run:      run,
		hc:       hc,
		URL:      UsageURL,
		Now:      time.Now,
		Getenv:   os.LookupEnv,
		Home:     config.Home(),
		GOOS:     runtime.GOOS,
		Location: time.Local,
		PasswdName: func() string {
			u, err := user.Current()
			if err != nil {
				return ""
			}
			return u.Username
		},
	}
}

// Provider implements model.Reader.
func (r *Reader) Provider() string { return model.Claude }

// Accounts implements model.Reader: one account per configured profile.
func (r *Reader) Accounts(ctx context.Context) ([]model.AccountRef, error) {
	var out []model.AccountRef
	for _, p := range r.cfg.Claude.Profiles {
		out = append(out, model.AccountRef{
			ID:       model.ID(model.Claude, p.Name),
			Provider: model.Claude,
			Name:     p.Name,
			Extra:    map[string]string{"config_dir": p.ConfigDir},
		})
	}
	return out, nil
}

func (r *Reader) getenv(name string) (string, bool) { return r.Getenv(name) }
func (r *Reader) passwdName() string {
	if r.PasswdName == nil {
		return ""
	}
	n := r.PasswdName()
	if !keychainAccountRE.MatchString(n) {
		return ""
	}
	return n
}
func (r *Reader) clock() time.Time {
	if r.clockFn != nil {
		return r.clockFn()
	}
	return r.Now()
}

// fetchResult is claude_code_usage.py's FetchResult.
type fetchResult struct {
	payload     *value
	fetchedAt   float64
	fromCache   bool
	staleReason *string
	source      string // api | config-cache
}

// configCacheResult reads the profile's own `cachedUsageUtilization`, the
// whole usage payload Claude Code records in its .claude.json.
func configCacheResult(path string) *fetchResult {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := parseValue(data)
	if err != nil || !v.isObject() {
		return nil
	}
	cached := v.get("cachedUsageUtilization")
	if !cached.isObject() {
		return nil
	}
	payload := cached.get("utilization")
	if !payload.isObject() {
		return nil
	}
	fetched := 0.0
	if ms := cached.get("fetchedAtMs"); ms.isNumber() {
		fetched = ms.float() / 1000
	}
	return &fetchResult{payload: payload, fetchedAt: fetched, fromCache: true, source: "config-cache"}
}

func (r *Reader) profile(name string) (config.ClaudeProfile, bool) {
	for _, p := range r.cfg.Claude.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return config.ClaudeProfile{}, false
}

// Read implements model.Reader.
//
// Contract with the daemon: on success the error is nil. When the token
// lookup or the fetch fails but the profile's own config cache has data,
// Read returns that snapshot (Source "config-cache") together with the
// error. With no data at all it returns an error snapshot (Source "") and
// the error, except when there is no credential anywhere, which is the
// signed_out verdict and no error.
func (r *Reader) Read(ctx context.Context, ref model.AccountRef, opts model.ReadOptions) (model.Snapshot, error) {
	prof, ok := r.profile(ref.Name)
	if !ok {
		return model.Snapshot{}, fmt.Errorf("%w: %s", model.ErrUnknownAccount, ref.ID)
	}
	rr := *r
	rr.home = r.Home
	rr.goos = r.GOOS
	rr.timeout = time.Duration(r.cfg.Claude.TimeoutS) * time.Second
	rr.keychainTimeout = time.Duration(r.cfg.Claude.KeychainTimeoutS) * time.Second
	if opts.Now != nil {
		rr.clockFn = opts.Now
	}
	return rr.read(ctx, prof)
}

func (r *Reader) read(ctx context.Context, prof config.ClaudeProfile) (model.Snapshot, error) {
	now := r.clock()
	pp := r.paths(prof.ConfigDir)
	kc := keychainInfo{
		Service: keychainServiceFor(pp.configDir, r.getenv),
		Account: r.keychainAccountCandidates()[0],
	}
	tok, failure := r.getToken(ctx, prof.Name, prof.TokenEnv, pp)
	var res *fetchResult
	if failure == nil {
		payload, err := r.fetch(ctx, tok.token)
		if err != nil {
			failure = err
		} else {
			res = &fetchResult{payload: payload, fetchedAt: model.Unix(now), source: "api"}
		}
	}
	var warnings []string
	if tok != nil && tok.warning != "" {
		warnings = append(warnings, tok.warning)
	}
	if res == nil {
		res = configCacheResult(pp.configJSON)
		if res == nil {
			snap := r.baseSnapshot(prof.Name)
			msg := reason(failure)
			snap.Native = mustJSON(errorRow{Profile: strPtrOrNil(prof.Name), Error: msg, Keychain: kc})
			var te *tokenError
			if errors.As(failure, &te) && te.noCredential {
				snap.Verdict = model.SignedOut
				snap.Notes = []model.Note{{Severity: model.Info, Text: msg}}
				return snap, nil
			}
			snap.Error = &msg
			return snap, failure
		}
		msg := reason(failure)
		res.staleReason = &msg
	}
	row := buildRow(prof.Name, tok, res, warnings, kc, model.Unix(now), r.Location)
	snap := r.normalize(prof.Name, tok, res, warnings, model.Unix(now))
	snap.Native = mustJSON(row)
	if failure != nil {
		msg := reason(failure)
		snap.Error = &msg
	}
	return snap, failure
}

func (r *Reader) baseSnapshot(name string) model.Snapshot {
	return model.Snapshot{
		ID:             model.ID(model.Claude, name),
		Provider:       model.Claude,
		Name:           name,
		Label:          name,
		Verdict:        model.Unknown,
		BlockedReasons: []string{},
		Windows:        []model.Window{},
		Notes:          []model.Note{},
	}
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err) // only fixed types are marshalled here
	}
	return data
}

// errorRow is build_json's object for a profile with nothing to show.
type errorRow struct {
	Profile  *string      `json:"profile"`
	Error    string       `json:"error"`
	Keychain keychainInfo `json:"keychain"`
	Relogin  *string      `json:"relogin"`
}

// legacyWindow is one entry of build_json's `windows`.
type legacyWindow struct {
	Key                string   `json:"key"`
	Label              string   `json:"label"`
	UtilizationPercent *float64 `json:"utilization_percent"`
	ResetsAt           *float64 `json:"resets_at"`
	ResetsAtISO        *string  `json:"resets_at_iso"`
	Severity           *string  `json:"severity"`
	IsActive           *bool    `json:"is_active"`
}

type legacyCache struct {
	FromCache   bool     `json:"from_cache"`
	StaleReason *string  `json:"stale_reason"`
	FetchedAt   *float64 `json:"fetched_at"`
}

// legacyRow is build_json's object for a profile with data, keys in the
// Python order. TokenExpiresAt is an addition: the human report's "OAuth
// token expired (3h ago)" line needs the expiry, which the old row lacked.
type legacyRow struct {
	Profile        *string         `json:"profile"`
	Error          *string         `json:"error"`
	Source         string          `json:"source"`
	Keychain       keychainInfo    `json:"keychain"`
	Warnings       []string        `json:"warnings"`
	Plan           *string         `json:"plan"`
	TokenSource    *string         `json:"token_source"`
	TokenExpired   *bool           `json:"token_expired"`
	TokenExpiresAt *float64        `json:"token_expires_at"`
	Relogin        *string         `json:"relogin"`
	Windows        []legacyWindow  `json:"windows"`
	ExtraUsage     json.RawMessage `json:"extra_usage"`
	CloudCredit    *cloudCredit    `json:"cloud_credit"`
	Cache          legacyCache     `json:"cache"`
	Raw            json.RawMessage `json:"raw"`
}

func buildRow(name string, tok *tokenInfo, res *fetchResult, warnings []string, kc keychainInfo, now float64, loc *time.Location) legacyRow {
	row := legacyRow{
		Profile:  strPtrOrNil(name),
		Source:   res.source,
		Keychain: kc,
		Warnings: append([]string{}, warnings...),
		Windows:  []legacyWindow{},
		Raw:      res.payload.raw(),
	}
	if tok != nil {
		row.Plan = tok.subscription
		row.TokenSource = &tok.source
		row.TokenExpired = tok.expired(now)
		row.TokenExpiresAt = tok.expiresAt
	}
	for _, w := range extractWindows(res.payload, loc) {
		lw := legacyWindow{Key: w.key, Label: w.label, UtilizationPercent: w.pct, ResetsAt: w.resetsAt,
			Severity: w.severity, IsActive: w.isActive}
		if w.resetsAt != nil {
			s := isoformat(*w.resetsAt, loc)
			lw.ResetsAtISO = &s
		}
		row.Windows = append(row.Windows, lw)
	}
	if eu := res.payload.get("extra_usage"); eu != nil {
		row.ExtraUsage = eu.raw()
	}
	row.CloudCredit = extractCloudCredit(res.payload, now, loc)
	fetched := res.fetchedAt
	row.Cache = legacyCache{FromCache: res.fromCache, StaleReason: res.staleReason, FetchedAt: &fetched}
	return row
}
