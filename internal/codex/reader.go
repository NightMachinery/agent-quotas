// Package codex reads Codex accounts' rate limits through `codex
// app-server`, as ~/scripts/python/codex_status.py does, and reproduces that
// script's JSON document and human report for `agent-quotas codex-status`.
//
// Each auth snapshot (auth_<alias>.json in a CODEX_HOME, plus the bare
// auth.json when it matches no snapshot) is one account. A read copies the
// snapshot into a temporary CODEX_HOME, so the app-server never touches the
// real one; whatever it writes there (a refreshed token) is discarded with
// the directory, exactly as the Python does. The one write back is the
// refresh-token recovery: when the snapshot's refresh token was already used,
// a newer auth with the same identity from auth_backup_dir is tried, and on
// success copied over the snapshot.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// Extra keys of a Codex AccountRef.
const (
	ExtraHome      = "home"       // configured home name
	ExtraCodexHome = "codex_home" // expanded CODEX_HOME
	ExtraAuthFile  = "auth_file"  // the snapshot file
	ExtraAlias     = "alias"
)

// Reader implements model.Reader for Codex.
type Reader struct {
	cfg *config.Config
	run procx.Runner

	// Launcher starts app-servers; ExecLauncher by default.
	Launcher Launcher
	// Now is the clock; time.Now by default.
	Now func() time.Time
	// Home is the home directory display paths are relative to.
	Home string
	// CdDir is passed as `codex --cd`; ~/tmp by default, as in the Python.
	CdDir string
	// Env is the base environment; nil means os.Environ().
	Env []string
}

// NewReader returns a Reader. run is kept for symmetry with the other
// readers: app-server needs a two-way pipe, so sessions go through Launcher.
func NewReader(cfg *config.Config, run procx.Runner) *Reader {
	return &Reader{cfg: cfg, run: run, Launcher: ExecLauncher{}, Now: time.Now}
}

// Provider is "codex".
func (r *Reader) Provider() string { return model.Codex }

func (r *Reader) home() string {
	if r.Home != "" {
		return r.Home
	}
	return config.Home()
}

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reader) fullPct() float64 {
	if r.cfg != nil && r.cfg.Codex.FullPct != 0 {
		return r.cfg.Codex.FullPct
	}
	return DefaultFullPct
}

// accountName is the name part of the id: the alias for the home named
// "default", <home>/<alias> otherwise.
func accountName(home, alias string) string {
	if home == "default" {
		return alias
	}
	return home + "/" + alias
}

// Accounts lists every auth snapshot of every configured home. An alias seen
// twice in one home (auth_x.json and auth-x.json) keeps its first file.
func (r *Reader) Accounts(ctx context.Context) ([]model.AccountRef, error) {
	var out []model.AccountRef
	seen := map[string]bool{}
	for _, h := range r.cfg.Codex.Homes {
		dir := expandUser(h.CodexHome, r.home())
		for _, src := range authSources(dir, r.home()) {
			id := model.ID(model.Codex, accountName(h.Name, src.Label))
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, model.AccountRef{
				ID: id, Provider: model.Codex, Name: accountName(h.Name, src.Label),
				Extra: map[string]string{ExtraHome: h.Name, ExtraCodexHome: dir, ExtraAuthFile: src.Path, ExtraAlias: src.Label},
			})
		}
	}
	return out, nil
}

// resolve fills a ref's locator from Extra, or by rediscovery.
func (r *Reader) resolve(ctx context.Context, ref model.AccountRef) (model.AccountRef, error) {
	if ref.Extra[ExtraAuthFile] != "" && ref.Extra[ExtraCodexHome] != "" {
		return ref, nil
	}
	refs, err := r.Accounts(ctx)
	if err != nil {
		return ref, err
	}
	for _, x := range refs {
		if x.ID == ref.ID {
			return x, nil
		}
	}
	return ref, fmt.Errorf("%w: %s", model.ErrUnknownAccount, ref.ID)
}

func (r *Reader) env(codexHome string) []string {
	env := r.Env
	if env == nil {
		env = os.Environ()
	}
	env = procx.EnvWithout(env, procx.AgentMarkers, nil)
	if r.cfg != nil {
		for k, v := range r.cfg.Env {
			env = procx.EnvSet(env, k, v)
		}
	}
	env = procx.EnvSet(env, "CODEX_HOME", codexHome)
	env = procx.EnvSet(env, "codex_home", codexHome)
	return env
}

func (r *Reader) argv() []string {
	bin := "codex"
	if r.cfg != nil && r.cfg.Codex.Binary != "" {
		bin = config.Expand(r.cfg.Codex.Binary)
	}
	cd := r.CdDir
	if cd == "" {
		cd = filepath.Join(r.home(), "tmp")
	}
	return []string{bin, "--cd", cd, "app-server"}
}

func (r *Reader) timeout() time.Duration {
	if r.cfg != nil && r.cfg.Codex.TimeoutS > 0 {
		return time.Duration(r.cfg.Codex.TimeoutS) * time.Second
	}
	return 30 * time.Second
}

func (r *Reader) backupDir() string {
	if r.cfg != nil && r.cfg.Codex.AuthBackupDir != "" {
		return expandUser(r.cfg.Codex.AuthBackupDir, r.home())
	}
	return filepath.Join(r.home(), "tmp", ".codex-auths")
}

// readResult is one gathered status plus what the snapshot needs beyond it.
type readResult struct {
	status authStatus
	// refreshedDiscarded: the isolated app-server rewrote auth.json (it
	// refreshed the token) and the copy was dropped, as the Python does.
	refreshedDiscarded bool
}

// gatherForAuthFile is gather_status_for_auth_file.
func (r *Reader) gatherForAuthFile(ctx context.Context, src authSource, authFile, configHome string, recovered *authSource) (readResult, error) {
	tmp, err := os.MkdirTemp("", "codex-status-")
	if err != nil {
		return readResult{}, err
	}
	defer os.RemoveAll(tmp)
	if err := prepareCodexHome(configHome, tmp, authFile); err != nil {
		return readResult{}, err
	}
	before, _ := fileBytes(filepath.Join(tmp, "auth.json"))
	sess, err := runSession(ctx, r.Launcher, r.argv(), r.env(tmp), r.timeout())
	if err != nil {
		return readResult{}, err
	}
	after, ok := fileBytes(filepath.Join(tmp, "auth.json"))
	res := readResult{refreshedDiscarded: ok && string(after) != string(before)}
	res.status = buildSuccessStatus(src, sess, authFile, recovered)
	return res, nil
}

// buildSuccessStatus is build_success_status for a status read from a file.
func buildSuccessStatus(src authSource, sess sessionResult, authFile string, recovered *authSource) authStatus {
	rl := newObj()
	if v, ok := sess.Rate.Get("rateLimits"); ok {
		if o, isO := v.(*obj); isO {
			rl = o
		}
	} else {
		rl = sess.Rate
	}
	claims := readAuthClaims(authFile)
	return authStatus{
		Source:        src,
		OK:            true,
		RateResult:    sess.Rate,
		AccountResult: sess.Account,
		RateLimits:    rl,
		Identity:      buildIdentity(rl, sess.Account, sess.Rate, claims, authFile),
		RecoveredFrom: recovered,
	}
}

// gatherOnce is gather_status_once for a source with a path.
func (r *Reader) gatherOnce(ctx context.Context, src authSource) (readResult, error) {
	res, err := r.gatherForAuthFile(ctx, src, src.Path, filepath.Dir(src.Path), nil)
	if err == nil {
		return res, nil
	}
	var se *statusError
	if !errors.As(err, &se) || !refreshTokenAlreadyUsed(se.msg, se.stderr) {
		return res, err
	}
	recPath := findRecoveredAuthFile(src.Path, r.backupDir())
	if recPath == "" {
		return res, err
	}
	label := aliasFromAuthPath(recPath)
	if label == "" {
		label = filepath.Base(recPath)
	}
	rec := &authSource{Path: recPath, Label: label, DisplayPath: homeRelative(recPath, r.home())}
	res, err = r.gatherForAuthFile(ctx, src, recPath, filepath.Dir(src.Path), rec)
	if err != nil {
		return res, err
	}
	if _, err := replaceAuthFile(recPath, src.Path); err != nil {
		return readResult{}, err
	}
	return res, nil
}

// errorStatus is error_status_from_exception.
func errorStatus(src authSource, err error) authStatus {
	var se *statusError
	if errors.As(err, &se) {
		return authStatus{Source: src, Error: se.msg, HasError: true, Stderr: se.stderr}
	}
	return authStatus{Source: src, Error: "codex-status: " + err.Error(), HasError: true}
}

var (
	reRateLimited = regexp.MustCompile(`(?i)\b429\b|too many requests|rate[ _-]?limited`)
	reAuthFailure = regexp.MustCompile(`(?i)\b401\b|unauthori[sz]ed|refresh token|invalid_grant|not logged in|log ?in again|sign ?in again`)
)

// classify turns a failed read into the error the daemon backs off from: a
// 429 or a timeout is an *model.UpstreamError; an auth failure, a missing
// binary or a local error is returned as is.
func classify(err error) error {
	var se *statusError
	if !errors.As(err, &se) {
		return err
	}
	text := se.msg + "\n" + se.stderr
	switch {
	case reRateLimited.MatchString(text):
		return &model.UpstreamError{Status: 429, Err: err}
	case reAuthFailure.MatchString(text):
		return err
	case se.msg == ErrCodexNotFound.Error():
		return fmt.Errorf("%w", ErrCodexNotFound)
	case se.timeout || strings.Contains(se.msg, " failed: "):
		return &model.UpstreamError{Status: 0, Err: err}
	}
	return err
}

// Read reads one account. On failure it returns the error and a Snapshot
// whose Native is the Python's error row (ok: false), so a caller can still
// show it.
func (r *Reader) Read(ctx context.Context, ref model.AccountRef, opts model.ReadOptions) (model.Snapshot, error) {
	now := r.now()
	if opts.Now != nil {
		now = opts.Now()
	}
	ref, err := r.resolve(ctx, ref)
	if err != nil {
		return model.Snapshot{}, err
	}
	authFile := ref.Extra[ExtraAuthFile]
	src := authSource{Path: authFile, Label: ref.Extra[ExtraAlias], DisplayPath: homeRelative(authFile, r.home())}
	if src.Label == "" {
		src.Label = aliasFromAuthPath(authFile)
	}
	if !isRegular(authFile) {
		snap := r.baseSnapshot(ref, now)
		snap.Verdict = model.SignedOut
		snap.Error = model.S("auth file missing: " + src.DisplayPath)
		return snap, nil
	}
	res, err := r.gatherOnce(ctx, src)
	if err != nil {
		st := errorStatus(src, err)
		snap := r.baseSnapshot(ref, now)
		snap.Error = model.S(st.Error)
		snap.Native = dumpTight(statusJSON(st, display{FullPct: r.fullPct()}))
		snap.Active = model.B(r.isActive(ref, st))
		return snap, classify(err)
	}
	return r.snapshot(ref, res, now), nil
}

func (r *Reader) baseSnapshot(ref model.AccountRef, now time.Time) model.Snapshot {
	return model.Snapshot{
		ID: ref.ID, Provider: model.Codex, Name: ref.Name, Label: ref.Name,
		Verdict: model.Unknown, BlockedReasons: []string{}, Windows: []model.Window{}, Notes: []model.Note{},
		FetchedAt: model.F(model.Unix(now)), Source: "app-server",
	}
}

// isActive is status_is_active against the ref's own CODEX_HOME.
func (r *Reader) isActive(ref model.AccountRef, st authStatus) bool {
	active := filepath.Join(ref.Extra[ExtraCodexHome], "auth.json")
	if !isRegular(active) {
		return false
	}
	for _, p := range st.authPaths() {
		if authBytesEqual(active, p) {
			return true
		}
	}
	return false
}

func (s authStatus) authPaths() []string {
	var out []string
	if s.Source.Path != "" {
		out = append(out, s.Source.Path)
	}
	if s.RecoveredFrom != nil && s.RecoveredFrom.Path != "" {
		out = append(out, s.RecoveredFrom.Path)
	}
	return out
}

// snapshot normalizes a successful status.
func (r *Reader) snapshot(ref model.AccountRef, res readResult, now time.Time) model.Snapshot {
	st := res.status
	d := display{FullPct: r.fullPct()}
	snap := r.baseSnapshot(ref, now)
	snap.Native = dumpTight(statusJSON(st, d))
	snap.Active = model.B(r.isActive(ref, st))
	fillSnapshot(&snap, st, d, now)
	if res.refreshedDiscarded {
		snap.Notes = append(snap.Notes, model.Note{Severity: model.Warning,
			Text: "app-server rewrote auth.json in the isolated copy (token refresh); the copy was discarded, as codex_status.py does"})
	}
	return snap
}

// SnapshotFromNative builds the normalized fields of a snapshot from one
// authFiles row (Snapshot.Native or a saved report entry). ID, name, poll
// and source are left to the caller.
func SnapshotFromNative(row json.RawMessage, fullPct float64, now time.Time) (model.Snapshot, error) {
	v, err := parseJSON(row)
	if err != nil {
		return model.Snapshot{}, err
	}
	o, ok := v.(*obj)
	if !ok {
		return model.Snapshot{}, errors.New("codex: row is not an object")
	}
	st := statusFromRow(o, "")
	d := display{FullPct: fullPct}
	snap := model.Snapshot{Provider: model.Codex, Verdict: model.Unknown, BlockedReasons: []string{},
		Windows: []model.Window{}, Notes: []model.Note{}, Native: dumpTight(statusJSON(st, d))}
	if !st.OK {
		snap.Error = model.S(st.Error)
		return snap, nil
	}
	fillSnapshot(&snap, st, d, now)
	return snap, nil
}

func fillSnapshot(snap *model.Snapshot, st authStatus, d display, now time.Time) {
	state := st.blockState(d)
	if state.Blocked {
		snap.Verdict = model.Blocked
		snap.BlockedReasons = append([]string{}, state.Reasons...)
		snap.BlockedUntil = state.ResetsAt
	} else {
		snap.Verdict = model.Usable
	}
	if v := asNonEmptyStr(st.Identity.V("planType")); v != "" {
		snap.Plan = model.S(v)
	}
	if v := asNonEmptyStr(st.Identity.V("planOwnerEmail")); v != "" {
		snap.Email = model.S(v)
	}
	if v := asNonEmptyStr(st.Identity.V("workspaceName")); v != "" {
		snap.Label = v
	} else if st.Source.Label != "" && snap.Label == "" {
		snap.Label = st.Source.Label
	}

	lim, effKey := st.codexLimit()
	prefix := effKey
	if prefix == "" {
		prefix = DefaultLimitID
	}
	snap.Windows = append(snap.Windows, normWindows(iterWindows(lim), prefix, "Codex", "", model.ScopeAccount, d.FullPct)...)
	_, reserveKey := lunaReserveLimit(st.rateResult())
	for _, v := range limitsByID(st.rateResult()) {
		switch {
		case effKey != "" && v.Key == effKey:
		case reserveKey != "" && v.Key == reserveKey:
		default:
			name := v.displayName()
			snap.Windows = append(snap.Windows, normWindows(v.Windows, v.Key, name, name+" ", model.ScopeModel, d.FullPct)...)
		}
	}
	if rv := lunaReserveView(st.rateResult()); rv != nil {
		snap.Windows = append(snap.Windows, normWindows(rv.Windows, rv.Key, LunaReserveLabel, LunaReserveLabel+" ", model.ScopeReserve, d.FullPct)...)
		rs := computeBlockState(rv.Raw, nil, d.FullPct)
		if rs.Blocked {
			snap.Notes = append(snap.Notes, model.Note{Severity: model.Warning, Text: LunaReserveLabel + " [" + LunaReserveModel + "] spent"})
		} else {
			snap.Notes = append(snap.Notes, model.Note{Severity: model.Info, Text: LunaReserveLabel + " [" + LunaReserveModel + "] available"})
		}
	}
	if line := formatCredits(plainStyle, asObj(lim.V("credits"))); line != "" {
		snap.Notes = append(snap.Notes, model.Note{Severity: model.Info, Text: "Credits: " + line})
	}
	if rc := getResetCredits(st.rateResult()); rc.AvailableCount > 0 {
		sev := model.Info
		text := fmt.Sprintf("Reset credits: %d available", rc.AvailableCount)
		var titles []string
		seen := map[string]bool{}
		var soonest *float64
		for _, c := range rc.availableCredits() {
			if t := c.Title; truthy(t) && !seen[pyStr(t)] {
				seen[pyStr(t)] = true
				titles = append(titles, pyStr(t))
			}
			if c.ExpiresAt != nil && (soonest == nil || *c.ExpiresAt < *soonest) {
				e := *c.ExpiresAt
				soonest = &e
			}
		}
		if len(titles) > 0 {
			text += " (" + strings.Join(titles, ", ")
			if soonest != nil {
				text += ", expires " + model.Time(*soonest).UTC().Format(time.RFC3339)
				if days := (*soonest - model.Unix(now)) / 86400; days <= 7 {
					sev = model.Warning
				}
			}
			text += ")"
		}
		snap.Notes = append(snap.Notes, model.Note{Severity: sev, Text: text})
	}
	if st.RecoveredFrom != nil {
		snap.Notes = append(snap.Notes, model.Note{Severity: model.Info, Text: "recovered auth from " + st.RecoveredFrom.DisplayPath})
	}
}

// normWindows maps rate-limit windows to model windows. Keys are
// <limit>-<minutes> (codex-300, codex-10080), or <limit>-<slot> when a
// window reports no duration.
func normWindows(ws []rlWindow, prefix, family, labelPrefix, scope string, fullPct float64) []model.Window {
	var out []model.Window
	for _, w := range ws {
		key := prefix + "-" + w.Key
		window := w.Key
		if w.DurationMins != nil && w.Label != "?" {
			key = prefix + "-" + strconv.FormatFloat(*w.DurationMins, 'f', -1, 64)
			window = strings.ToLower(w.Label)
		}
		out = append(out, model.Window{
			Key: key, Family: family, Window: window, Label: labelPrefix + w.Label,
			UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt, WindowMinutes: w.DurationMins,
			Scope: scope, Blocking: w.blocked(fullPct),
		})
	}
	return out
}

// UsableFor is the Paseo watcher's quota_usable for Codex: whether the
// account's regular allowance is usable. It errors when the verdict is
// unavailable (a failed read, or no .quota.blocked boolean).
func UsableFor(s model.Snapshot) (bool, error) {
	if len(s.Native) == 0 {
		switch s.Verdict {
		case model.Usable:
			return true, nil
		case model.Blocked:
			return false, nil
		}
		return false, errors.New("Codex quota check failed")
	}
	v, err := parseJSON(s.Native)
	if err != nil {
		return false, errors.New("Codex quota check failed")
	}
	row := asObj(v)
	if ok, isB := row.V("ok").(bool); !isB || !ok || !isObj(row.V("quota")) {
		return false, errors.New("Codex quota check failed")
	}
	blocked, isB := asObj(row.V("quota")).V("blocked").(bool)
	if !isB {
		return false, errors.New("Codex quota verdict unavailable")
	}
	return !blocked, nil
}
