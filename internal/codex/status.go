package codex

// The per-auth status of codex_status.py and its JSON forms: status_json
// (one entry of authFiles, also Snapshot.Native), quota_json and the
// averageUsage aggregate.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// authSource names one auth file being checked.
type authSource struct {
	Path        string // absolute path, "" for none
	Label       string // alias
	DisplayPath string // ~/... form, "" for none
}

// authStatus is codex_status.AuthStatus.
type authStatus struct {
	Source        authSource
	OK            bool
	RateResult    *obj // the account/rateLimits/read result
	AccountResult *obj // the account/read result
	RateLimits    *obj
	Identity      *obj // planType, planOwnerEmail, workspaceName
	Error         string
	HasError      bool
	Stderr        string
	RecoveredFrom *authSource
}

// display is codex_status.Display with the defaults the daemon uses: every
// section on, no --limits regex.
type display struct {
	FullPct float64
}

func (s authStatus) rateResult() *obj {
	if s.RateResult == nil {
		return newObj()
	}
	return s.RateResult
}

// codexLimit is status_codex_limit: the limit governing ordinary usage, and
// the rateLimitsByLimitId key it was taken from ("" when it was not).
func (s authStatus) codexLimit() (*obj, string) {
	lim, key := getCodexLimit(s.rateResult())
	if lim.Len() > 0 {
		return lim, key
	}
	if s.RateLimits.Len() > 0 {
		return s.RateLimits, ""
	}
	return newObj(), ""
}

func (s authStatus) blockState(d display) blockState {
	lim, _ := s.codexLimit()
	return computeBlockState(lim, s.rateResult(), d.FullPct)
}

func (s authStatus) usable(d display) bool { return !s.blockState(d).Blocked }

// limitIsInteresting: non-idle or blocked families earn a line (there is no
// --limits regex in the port; --show-limits is on).
func limitIsInteresting(v limitView, d display) bool {
	if computeBlockState(v.Raw, nil, d.FullPct).Blocked {
		return true
	}
	for _, w := range v.Windows {
		if w.UsedPercent != nil && *w.UsedPercent > 0 {
			return true
		}
	}
	return false
}

func (s authStatus) reportableLimits(d display) []limitView {
	_, effKey := s.codexLimit()
	_, reserveKey := lunaReserveLimit(s.rateResult())
	var out []limitView
	for _, v := range limitsByID(s.rateResult()) {
		if (effKey != "" && v.Key == effKey) || (reserveKey != "" && v.Key == reserveKey) {
			continue
		}
		if limitIsInteresting(v, d) {
			out = append(out, v)
		}
	}
	return out
}

func quotaJSON(s authStatus, d display) *obj {
	lim, _ := s.codexLimit()
	st := s.blockState(d)
	view := newLimitView(lim, "")
	p := newObj().
		Set("source", limitSource(s.rateResult())).
		Set("limitId", strOrNil(view.LimitID)).
		Set("limitName", strOrNil(view.LimitName)).
		Set("normalModelSlug", strOrNil(view.NormalModelSlug)).
		Set("fullPct", d.FullPct).
		Set("blocked", st.Blocked).
		Set("reasons", reasonsJSON(st.Reasons)).
		Set("resetsAt", jsonNumber(st.ResetsAt)).
		Set("windows", windowsJSON(view.Windows, d.FullPct))
	if r := lunaReserveJSON(s.rateResult(), d.FullPct); r != nil {
		p.Set("lunaReserve", r)
	}
	p.Set("signals", signalsJSON(lim, s.rateResult()))
	p.Set("resetCredits", resetCreditsJSON(getResetCredits(s.rateResult())))
	limits := []any{}
	for _, v := range s.reportableLimits(d) {
		limits = append(limits, limitJSON(v, d.FullPct))
	}
	p.Set("limits", limits)
	return p
}

// statusJSON is codex_status.status_json: the raw response spread verbatim,
// then account, identity, the interpreted quota, and the bookkeeping keys.
func statusJSON(s authStatus, d display) *obj {
	p := newObj()
	if s.OK {
		rr := s.rateResult()
		for _, k := range rr.Keys() {
			p.Set(k, rr.V(k))
		}
		if acc := asObj(s.AccountResult).V("account"); acc != nil {
			p.Set("account", acc)
		}
		if s.Identity.Len() > 0 {
			p.Set("identity", s.Identity)
		}
		p.Set("quota", quotaJSON(s, d))
	} else {
		msg := s.Error
		if msg == "" {
			msg = "unknown error"
		}
		p.Set("error", msg)
		if st := pyStrip(s.Stderr); st != "" {
			p.Set("stderr", st)
		}
	}
	p.Set("ok", s.OK)
	p.Set("alias", s.Source.Label)
	if s.Source.DisplayPath != "" {
		p.Set("authFile", s.Source.DisplayPath)
	}
	if s.RecoveredFrom != nil {
		p.Set("recoveredFrom", newObj().
			Set("alias", s.RecoveredFrom.Label).
			Set("authFile", strOrNil(nonEmpty(s.RecoveredFrom.DisplayPath))))
	}
	return p
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// replayKeys are the keys statuses_from_report removes before treating the
// rest of an entry as the raw rate-limit response.
var replayKeys = map[string]bool{
	"ok": true, "alias": true, "authFile": true, "identity": true, "account": true,
	"error": true, "stderr": true, "recoveredFrom": true, "quota": true,
}

// statusFromRow rebuilds a status from one authFiles entry, as
// statuses_from_report does for --from-json. Unlike the replay, it keeps
// recoveredFrom, since rows here come from live reads where the Python
// prints it.
func statusFromRow(entry *obj, home string) authStatus {
	var src authSource
	if dp, ok := entry.V("authFile").(string); ok {
		src.DisplayPath = dp
		src.Path = expandUser(dp, home)
	}
	if a := entry.V("alias"); truthy(a) {
		src.Label = pyStr(a)
	} else {
		src.Label = "auth"
	}
	rr := newObj()
	for _, k := range entry.Keys() {
		if !replayKeys[k] {
			rr.Set(k, entry.V(k))
		}
	}
	st := authStatus{
		Source:     src,
		OK:         truthy(entry.V("ok")),
		RateResult: rr,
		RateLimits: asObj(rr.V("rateLimits")),
		Identity:   asObj(entry.V("identity")),
	}
	if acc := entry.V("account"); truthy(acc) {
		st.AccountResult = newObj().Set("account", acc)
	}
	if e := entry.V("error"); e != nil {
		st.Error, st.HasError = pyStr(e), true
	}
	if e := entry.V("stderr"); truthy(e) {
		st.Stderr = pyStr(e)
	}
	if rf := asObj(entry.V("recoveredFrom")); rf != nil {
		r := &authSource{Label: pyStr(rf.V("alias"))}
		if dp, ok := rf.V("authFile").(string); ok {
			r.DisplayPath = dp
			r.Path = expandUser(dp, home)
		}
		st.RecoveredFrom = r
	}
	return st
}

// statusesFromRows parses rows (each one authFiles entry) into statuses.
func statusesFromRows(rows []json.RawMessage, home string) ([]authStatus, error) {
	var out []authStatus
	for i, raw := range rows {
		v, err := parseJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		o, ok := v.(*obj)
		if !ok {
			continue
		}
		out = append(out, statusFromRow(o, home))
	}
	return out, nil
}

func averageUsedPercent(statuses []authStatus, slot string) *float64 {
	var values []float64
	for _, s := range statuses {
		if !s.OK {
			continue
		}
		if p := usedPercentOf(s.RateLimits.V(slot)); p != nil {
			values = append(values, *p)
		}
	}
	if len(values) == 0 {
		return nil
	}
	avg := pySum(values) / float64(len(values))
	return &avg
}

type resetCandidate struct {
	Status    authStatus
	ResetTime float64
}

func firstQuotaReset(statuses []authStatus, d display) *resetCandidate {
	var oks []authStatus
	for _, s := range statuses {
		if s.OK {
			oks = append(oks, s)
		}
	}
	if len(oks) == 0 {
		return nil
	}
	for _, s := range oks {
		if s.usable(d) {
			return nil
		}
	}
	var best *resetCandidate
	less := func(a, b resetCandidate) bool {
		if a.ResetTime != b.ResetTime {
			return a.ResetTime < b.ResetTime
		}
		if a.Status.Source.Label != b.Status.Source.Label {
			return a.Status.Source.Label < b.Status.Source.Label
		}
		return a.Status.Source.DisplayPath < b.Status.Source.DisplayPath
	}
	for _, s := range oks {
		rt := s.blockState(d).ResetsAt
		if rt == nil {
			continue
		}
		c := resetCandidate{Status: s, ResetTime: *rt}
		if best == nil || less(c, *best) {
			best = &c
		}
	}
	return best
}

type avgRow struct {
	Label        string
	DurationMins *float64
	UsedPercent  float64
	AuthCount    int
}

// averageWindowsByDuration averages usage per window duration across auths.
func averageWindowsByDuration(statuses []authStatus) []avgRow {
	buckets := map[float64][]rlWindow{}
	for _, s := range statuses {
		if !s.OK {
			continue
		}
		lim, _ := s.codexLimit()
		for _, w := range iterWindows(lim) {
			if w.UsedPercent == nil {
				continue
			}
			key := -1.0
			if w.DurationMins != nil {
				key = *w.DurationMins
			}
			buckets[key] = append(buckets[key], w)
		}
	}
	durations := make([]float64, 0, len(buckets))
	for k := range buckets {
		durations = append(durations, k)
	}
	sort.Float64s(durations)
	var rows []avgRow
	for _, dur := range durations {
		ws := buckets[dur]
		values := make([]float64, 0, len(ws))
		for _, w := range ws {
			values = append(values, *w.UsedPercent)
		}
		row := avgRow{Label: ws[0].Label, UsedPercent: pySum(values) / float64(len(values)), AuthCount: len(values)}
		if dur >= 0 {
			d := dur
			row.DurationMins = &d
		}
		rows = append(rows, row)
	}
	return rows
}

func floatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func averageUsageJSON(statuses []authStatus, d display) *obj {
	p := newObj().
		Set("primaryUsedPercent", floatOrNil(averageUsedPercent(statuses, "primary"))).
		Set("secondaryUsedPercent", floatOrNil(averageUsedPercent(statuses, "secondary")))
	rows := averageWindowsByDuration(statuses)
	arr := []any{}
	var maxUsed *float64
	for _, r := range rows {
		arr = append(arr, newObj().
			Set("label", r.Label).
			Set("windowDurationMins", jsonNumber(r.DurationMins)).
			Set("usedPercent", r.UsedPercent).
			Set("authCount", r.AuthCount))
		if maxUsed == nil || r.UsedPercent > *maxUsed {
			v := r.UsedPercent
			maxUsed = &v
		}
	}
	p.Set("windows", arr)
	p.Set("maxUsedPercent", floatOrNil(maxUsed))
	usable, total := 0, 0
	for _, s := range statuses {
		if s.OK {
			total++
			if s.usable(d) {
				usable++
			}
		}
	}
	p.Set("usableCount", usable)
	p.Set("blockedCount", total-usable)
	if fr := firstQuotaReset(statuses, d); fr != nil {
		rt := fr.ResetTime
		p.Set("firstTimeToReset", jsonNumber(&rt))
		p.Set("firstTimeToResetAlias", fr.Status.Source.Label)
	}
	return p
}

// LegacyDocument reproduces codex_status.py's --json document from per-auth
// rows (Snapshot.Native values). With all, it is {"authFiles": [...],
// "averageUsage": {...}}; without, the first row alone ({} when there is
// none). Each row's quota is recomputed with fullPct, as the Python does when
// it replays a saved report with --full-pct.
func LegacyDocument(rows []json.RawMessage, all bool, now time.Time, fullPct float64) ([]byte, error) {
	_ = now // the document carries no clock-dependent field
	statuses, err := statusesFromRows(rows, "")
	if err != nil {
		return nil, err
	}
	d := display{FullPct: fullPct}
	var out any
	if all {
		files := []any{}
		for _, s := range statuses {
			files = append(files, statusJSON(s, d))
		}
		out = newObj().Set("authFiles", files).Set("averageUsage", averageUsageJSON(statuses, d))
	} else if len(statuses) > 0 {
		out = statusJSON(statuses[0], d)
	} else {
		out = newObj()
	}
	return append(dumpIndent(out), '\n'), nil
}

// pyStrip is str.strip().
func pyStrip(s string) string { return strings.TrimSpace(s) }
