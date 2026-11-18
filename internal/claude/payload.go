package claude

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// usageWindow is claude_code_usage.py's UsageWindow.
type usageWindow struct {
	key      string
	label    string
	pct      *float64
	resetsAt *float64
	raw      *value
	severity *string
	isActive *bool
	// scopeModel is the limit's scope.model.display_name, when it has one.
	scopeModel string
}

var windowLabels = map[string]string{
	"five_hour":        "5h session",
	"seven_day":        "7d (all models)",
	"seven_day_opus":   "7d Opus",
	"seven_day_sonnet": "7d Sonnet",
}

// windowOrder is the order of windowLabels in the Python source.
var windowOrder = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}

var limitKindLabels = map[string]string{
	"session":    "5h session",
	"weekly_all": "7d (all models)",
}

// cloudCreditKey is where the payload has been seen to carry the cloud
// session credit. The name is internal, so the credit is also found by shape.
const cloudCreditKey = "iguana_necktie"

// parseUtilization: the endpoint returns both 0..1 fractions and 0..100
// percentages. A float in [0, 1] is a fraction; integers pass through.
func parseUtilization(v *value) *float64 {
	if !v.isNumber() {
		return nil
	}
	n := v.float()
	if v.kind == 'f' && n >= 0 && n <= 1 {
		n *= 100
	}
	return &n
}

// parseResetsAt accepts epoch seconds, epoch milliseconds, or an ISO 8601
// string (a naive one is local time, as in Python).
func parseResetsAt(v *value, loc *time.Location) *float64 {
	if v == nil {
		return nil
	}
	switch v.kind {
	case 'i', 'f':
		n := v.float()
		if n > 1e12 {
			n /= 1000
		}
		return &n
	case 's':
		return parseISO(strings.Replace(v.s, "Z", "+00:00", -1), loc)
	}
	return nil
}

// parseISO is the subset of datetime.fromisoformat the endpoint can return:
// YYYY-MM-DD[(T| )HH[:MM[:SS[.fff...]]]][Z|±HH[[:]MM[[:]SS]]]. Fractions
// beyond microseconds are truncated, as Python does.
func parseISO(s string, loc *time.Location) *float64 {
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return nil
	}
	year, e1 := strconv.Atoi(s[0:4])
	month, e2 := strconv.Atoi(s[5:7])
	day, e3 := strconv.Atoi(s[8:10])
	if e1 != nil || e2 != nil || e3 != nil {
		return nil
	}
	rest := s[10:]
	var hour, minute, sec, usec int
	zone := loc
	if rest != "" {
		if rest[0] != 'T' && rest[0] != 't' && rest[0] != ' ' {
			return nil
		}
		rest = rest[1:]
		// Split off the offset.
		tz := ""
		if i := strings.IndexAny(rest, "+-"); i >= 0 {
			rest, tz = rest[:i], rest[i:]
		}
		clock, frac, hasFrac := strings.Cut(rest, ".")
		if !hasFrac {
			clock, frac, hasFrac = strings.Cut(rest, ",")
		}
		parts := strings.Split(clock, ":")
		if len(parts) > 3 || parts[0] == "" {
			return nil
		}
		nums := make([]int, 3)
		for i, p := range parts {
			if len(p) != 2 {
				return nil
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			nums[i] = n
		}
		hour, minute, sec = nums[0], nums[1], nums[2]
		if hasFrac {
			if frac == "" || len(parts) != 3 {
				return nil
			}
			for _, c := range frac {
				if c < '0' || c > '9' {
					return nil
				}
			}
			if len(frac) > 6 {
				frac = frac[:6]
			}
			frac += strings.Repeat("0", 6-len(frac))
			usec, _ = strconv.Atoi(frac)
		}
		if tz != "" {
			sign := 1
			if tz[0] == '-' {
				sign = -1
			}
			digits := strings.ReplaceAll(tz[1:], ":", "")
			if len(digits) != 2 && len(digits) != 4 && len(digits) != 6 {
				return nil
			}
			digits += strings.Repeat("0", 6-len(digits))
			h, _ := strconv.Atoi(digits[0:2])
			m, _ := strconv.Atoi(digits[2:4])
			sc, _ := strconv.Atoi(digits[4:6])
			zone = time.FixedZone("", sign*(h*3600+m*60+sc))
		}
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || sec > 59 {
		return nil
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, usec*1000, zone)
	if zone == loc {
		// Naive: Python adds the microseconds to mktime's whole seconds.
		f := float64(t.Unix()) + float64(usec)/1e6
		return &f
	}
	// Aware: timedelta.total_seconds(), one correctly rounded division.
	f := float64(t.Unix()*1_000_000+int64(usec)) / 1e6
	return &f
}

func looksLikeWindow(v *value) bool {
	if !v.isObject() {
		return false
	}
	_, a := v.obj["utilization"]
	_, b := v.obj["resets_at"]
	return a || b
}

func windowLabel(key string) string {
	if l, ok := windowLabels[key]; ok {
		return l
	}
	return strings.ReplaceAll(key, "_", " ")
}

func buildWindow(key string, raw *value, loc *time.Location) usageWindow {
	return usageWindow{
		key:      key,
		label:    windowLabel(key),
		pct:      parseUtilization(raw.get("utilization")),
		resetsAt: parseResetsAt(raw.get("resets_at"), loc),
		raw:      raw,
	}
}

// truthyStr is Python's `str(x or y or z)` for the kind/group fallbacks.
func firstTruthyStr(vals ...*value) string {
	for _, v := range vals {
		if v.truthy() {
			return v.pyStr()
		}
	}
	return ""
}

func limitLabel(limit *value) string {
	if k, ok := limit.get("kind").str(); ok {
		if base, ok := limitKindLabels[k]; ok {
			return base
		}
	}
	scopeName := ""
	if sv := limit.getPath("scope", "model", "display_name"); sv != nil && sv.truthy() {
		scopeName = sv.pyStr()
	}
	if g, ok := limit.get("group").str(); ok && g == "weekly" {
		if scopeName != "" {
			return "7d " + scopeName
		}
		return "7d (scoped)"
	}
	text := firstTruthyStr(limit.get("kind"), limit.get("group"))
	if text == "" {
		text = "limit"
	}
	text = strings.ReplaceAll(text, "_", " ")
	if scopeName != "" {
		return text + " " + scopeName
	}
	return text
}

func buildLimitWindow(limit *value, loc *time.Location) usageWindow {
	key := firstTruthyStr(limit.get("kind"), limit.get("group"))
	if key == "" {
		key = "limit"
	}
	w := usageWindow{
		key:      key,
		label:    limitLabel(limit),
		pct:      parseUtilization(limit.get("percent")),
		resetsAt: parseResetsAt(limit.get("resets_at"), loc),
		raw:      limit,
	}
	if s, ok := limit.get("severity").str(); ok {
		w.severity = &s
	}
	if b := limit.get("is_active"); b != nil && b.kind == 'b' {
		v := b.b
		w.isActive = &v
	}
	if sv := limit.getPath("scope", "model", "display_name"); sv != nil && sv.truthy() {
		w.scopeModel = sv.pyStr()
	}
	return w
}

// extractWindows prefers the authoritative `limits` array and falls back to
// the legacy window objects, unknown keys included.
func extractWindows(payload *value, loc *time.Location) []usageWindow {
	if limits := payload.get("limits"); limits != nil && limits.kind == 'a' {
		var out []usageWindow
		for _, l := range limits.arr {
			if l.isObject() {
				out = append(out, buildLimitWindow(l, loc))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	var out []usageWindow
	known := map[string]bool{}
	for _, k := range windowOrder {
		known[k] = true
		if v := payload.get(k); looksLikeWindow(v) {
			out = append(out, buildWindow(k, v, loc))
		}
	}
	if !payload.isObject() {
		return out
	}
	for _, k := range payload.sortedKeys() {
		v := payload.obj[k]
		if known[k] || k == "extra_usage" || looksLikeCredit(v) {
			continue
		}
		if looksLikeWindow(v) {
			out = append(out, buildWindow(k, v, loc))
		}
	}
	return out
}

// looksLikeCredit: the usage windows carry the same dollar fields set to
// null, so only numbers in both mark a credit.
func looksLikeCredit(v *value) bool {
	return v.isObject() && v.get("limit_dollars").isNumber() && v.get("remaining_dollars").isNumber()
}

// cloudCredit is extract_cloud_credit's normalized object. Field order is
// the Python dict's.
type cloudCredit struct {
	Key              string   `json:"key"`
	LimitDollars     float64  `json:"limit_dollars"`
	UsedDollars      *float64 `json:"used_dollars"`
	RemainingDollars float64  `json:"remaining_dollars"`
	ExpiresAt        *float64 `json:"expires_at"`
	ExpiresAtISO     *string  `json:"expires_at_iso"`
	LockedReason     *string  `json:"locked_reason"`
	Available        bool     `json:"available"`
}

func extractCloudCredit(payload *value, now float64, loc *time.Location) *cloudCredit {
	key := cloudCreditKey
	raw := payload.get(key)
	if !looksLikeCredit(raw) {
		raw = nil
		if payload.isObject() {
			for _, k := range payload.sortedKeys() {
				if looksLikeCredit(payload.obj[k]) {
					key, raw = k, payload.obj[k]
					break
				}
			}
		}
		if raw == nil {
			return nil
		}
	}
	c := &cloudCredit{
		Key:              key,
		LimitDollars:     raw.get("limit_dollars").float(),
		RemainingDollars: raw.get("remaining_dollars").float(),
		ExpiresAt:        parseResetsAt(raw.get("resets_at"), loc),
	}
	if u := raw.get("used_dollars"); u.isNumber() {
		f := u.float()
		c.UsedDollars = &f
	}
	if c.ExpiresAt != nil {
		s := isoformat(*c.ExpiresAt, loc)
		c.ExpiresAtISO = &s
	}
	if lr := raw.get("locked_reason"); lr.truthy() {
		s := lr.pyStr()
		c.LockedReason = &s
	}
	c.Available = c.RemainingDollars > 0 && (c.ExpiresAt == nil || *c.ExpiresAt > now)
	return c
}

// extraUsageLine is render_extra_usage without styling of the values: nil
// when extra usage is off and unused.
func extraUsageActive(extra *value) bool {
	if !extra.isObject() {
		return false
	}
	return extra.get("is_enabled").truthy() || extra.get("used_credits").truthy() || extra.get("spend_limit_reached").truthy()
}

func extraUsageDetail(extra *value) string {
	var bits []string
	for _, k := range extra.sortedKeys() {
		v := extra.obj[k]
		if v.isNull() {
			continue
		}
		bits = append(bits, k+": "+v.pyStr())
	}
	return strings.Join(bits, ", ")
}

// fromTimestamp is datetime.fromtimestamp(x) in loc: microseconds rounded
// half to even, as Python rounds.
func fromTimestamp(x float64, loc *time.Location) time.Time {
	ipart, frac := math.Modf(x)
	us := math.RoundToEven(frac * 1e6)
	sec := int64(ipart)
	if us >= 1e6 {
		sec++
		us -= 1e6
	} else if us < 0 {
		sec--
		us += 1e6
	}
	return time.Unix(sec, int64(us)*1000).In(loc)
}

// isoformat is datetime.fromtimestamp(x).astimezone().isoformat().
func isoformat(x float64, loc *time.Location) string {
	t := fromTimestamp(x, loc)
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	_, off := t.Zone()
	sign := '+'
	if off < 0 {
		sign = '-'
		off = -off
	}
	s += fmt.Sprintf("%c%02d:%02d", sign, off/3600, off/60%60)
	if off%60 != 0 {
		s += fmt.Sprintf(":%02d", off%60)
	}
	return s
}

// formatTimestamp is common_sub_status.format_timestamp.
func formatTimestamp(x float64, loc *time.Location) string {
	return fromTimestamp(x, loc).Format("2006-01-02 15:04:05 -0700")
}

// formatRelative is common_sub_status.format_relative.
func formatRelative(x float64, now float64) string {
	delta := int64(math.RoundToEven(x - now))
	past := delta < 0
	if past {
		delta = -delta
	}
	weeks, rem := delta/(7*86400), delta%(7*86400)
	days, rem := rem/86400, rem%86400
	hours, rem := rem/3600, rem%3600
	mins, secs := rem/60, rem%60
	var parts []string
	if weeks != 0 {
		parts = append(parts, fmt.Sprintf("%dw", weeks))
	}
	if days != 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours != 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins != 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if secs != 0 && len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	if len(parts) == 0 {
		parts = append(parts, "0s")
	}
	text := strings.Join(parts, " ")
	if past {
		return text + " ago"
	}
	return "in " + text
}
