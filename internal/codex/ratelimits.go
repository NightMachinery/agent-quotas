package codex

// Port of ~/scripts/python/libs/codex_rate_limits.py: the interpretation of
// Codex's account/rateLimits/read payload. Window shapes are discovered, not
// assumed: a plan may report one window or several, in any slot, and a
// missing window is never an exhausted one.

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// DefaultFullPct is the utilization at which a window counts as spent.
const DefaultFullPct = 100.0

// DefaultLimitID is the limit governing ordinary Codex usage. Other ids in
// rateLimitsByLimitId are per-model families and never decide usability.
const DefaultLimitID = "codex"

// The Luna Reserve: a second allowance for ordinary work, reported as just
// another entry of rateLimitsByLimitId. It never makes a blocked account
// usable; it is reported beside the plan's own windows.
const (
	LunaReserveLimitID = "base_model_inference"
	LunaReserveModel   = "gpt-reserve"
	LunaReserveLabel   = "Luna Reserve"
)

var nonWindowKeys = map[string]bool{"credits": true, "rateLimitUpsell": true, "account": true, "individualLimit": true}

var windowFields = []string{"usedPercent", "resetsAt", "windowDurationMins"}

var legacyWindowOrder = []string{"primary", "secondary"}

// rlWindow is one usage window of a limit.
type rlWindow struct {
	Key          string
	UsedPercent  *float64
	ResetsAt     *float64
	DurationMins *float64
	Label        string
}

func (w rlWindow) blocked(fullPct float64) bool {
	return w.UsedPercent != nil && *w.UsedPercent >= fullPct
}

func numPtr(v any) *float64 {
	if f, ok := numeric(v); ok {
		return &f
	}
	return nil
}

func usedPercentOf(window any) *float64 { return numPtr(asObj(window).V("usedPercent")) }

func resetTimeOf(window any) *float64 { return numPtr(asObj(window).V("resetsAt")) }

// formatWindowDuration labels a window by its duration: 5h, Weekly, 30m.
func formatWindowDuration(mins *float64) string {
	if mins == nil || *mins <= 0 {
		return "?"
	}
	total := int64(math.RoundToEven(*mins))
	switch {
	case total == 1440:
		return "Daily"
	case total == 10080:
		return "Weekly"
	case total == 43200 || total == 44640:
		return "Monthly"
	case total < 60:
		return fmt.Sprintf("%dm", total)
	case total < 1440:
		if total%60 == 0 {
			return fmt.Sprintf("%dh", total/60)
		}
		return pyG(float64(total)/60) + "h"
	case total%1440 == 0:
		return fmt.Sprintf("%dd", total/1440)
	}
	return fmt.Sprintf("%dm", total)
}

func isWindowShaped(key string, value any) bool {
	o, ok := value.(*obj)
	if nonWindowKeys[key] || !ok {
		return false
	}
	for _, f := range windowFields {
		if o.Has(f) {
			return true
		}
	}
	return false
}

func buildWindow(key string, raw any) rlWindow {
	data := asObj(raw)
	d := numPtr(data.V("windowDurationMins"))
	return rlWindow{
		Key:          key,
		UsedPercent:  usedPercentOf(data),
		ResetsAt:     resetTimeOf(data),
		DurationMins: d,
		Label:        formatWindowDuration(d),
	}
}

// iterWindows returns every usage window a limit reports: primary and
// secondary first, then the rest sorted by key. Empty means unconstrained.
func iterWindows(limit any) []rlWindow {
	data := asObj(limit)
	var keys []string
	for _, k := range legacyWindowOrder {
		if isWindowShaped(k, data.V(k)) {
			keys = append(keys, k)
		}
	}
	var rest []string
	for _, k := range data.Keys() {
		if k != "primary" && k != "secondary" && isWindowShaped(k, data.V(k)) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)
	out := make([]rlWindow, 0, len(keys))
	for _, k := range keys {
		out = append(out, buildWindow(k, data.V(k)))
	}
	return out
}

// limitView is one limit object with its windows.
type limitView struct {
	LimitID         *string
	LimitName       *string
	NormalModelSlug *string
	Windows         []rlWindow
	Raw             *obj
	// Key is the rateLimitsByLimitId key the view came from, "" otherwise.
	Key string
}

func (v limitView) displayName() string {
	switch {
	case v.LimitName != nil && *v.LimitName != "" && v.LimitID != nil && *v.LimitID != "":
		return *v.LimitName + " [" + *v.LimitID + "]"
	case v.LimitName != nil && *v.LimitName != "":
		return *v.LimitName
	case v.LimitID != nil && *v.LimitID != "":
		return *v.LimitID
	}
	return "?"
}

func strField(o *obj, k string) *string {
	if s, ok := o.V(k).(string); ok {
		return &s
	}
	return nil
}

func newLimitView(limit any, key string) limitView {
	data := asObj(limit)
	if data == nil {
		data = newObj()
	}
	return limitView{
		LimitID:         strField(data, "limitId"),
		LimitName:       strField(data, "limitName"),
		NormalModelSlug: strField(data, "normalModelSlug"),
		Windows:         iterWindows(data),
		Raw:             data,
		Key:             key,
	}
}

// unwrapAuthEnvelope picks one entry out of an {"authFiles": [...]} report.
func unwrapAuthEnvelope(status any, alias *string) *obj {
	entries, ok := asObj(status).V("authFiles").([]any)
	if !ok {
		return nil
	}
	var okEntries []*obj
	for _, e := range entries {
		if o, isO := e.(*obj); isO && truthy(o.V("ok")) {
			okEntries = append(okEntries, o)
		}
	}
	if len(okEntries) == 0 {
		return nil
	}
	if alias != nil {
		for _, e := range okEntries {
			if s, isS := e.V("alias").(string); isS && s == *alias {
				return e
			}
		}
	}
	for _, e := range okEntries {
		if truthy(e.V("active")) {
			return e
		}
	}
	return okEntries[0]
}

// getCodexLimit returns the limit governing ordinary usage, or nil, and the
// rateLimitsByLimitId key it came from ("" otherwise).
func getCodexLimit(status any) (*obj, string) {
	data := asObj(status)
	if d, ok := asObj(data.V("rateLimitsByLimitId")).V(DefaultLimitID).(*obj); ok {
		return d, DefaultLimitID
	}
	if top, ok := data.V("rateLimits").(*obj); ok {
		return top, ""
	}
	if entry := unwrapAuthEnvelope(data, nil); entry != nil {
		lim, _ := getCodexLimit(entry)
		return lim, ""
	}
	return nil, ""
}

func limitSource(status any) any {
	data := asObj(status)
	if isObj(asObj(data.V("rateLimitsByLimitId")).V(DefaultLimitID)) {
		return "rateLimitsByLimitId." + DefaultLimitID
	}
	if isObj(data.V("rateLimits")) {
		return "rateLimits"
	}
	if unwrapAuthEnvelope(data, nil) != nil {
		return "authFiles"
	}
	return nil
}

func isLunaReserve(limit any) bool {
	data := asObj(limit)
	if name, ok := data.V("limitName").(string); ok && strings.ToLower(strings.TrimSpace(name)) == LunaReserveModel {
		return true
	}
	id, ok := data.V("limitId").(string)
	return ok && id == LunaReserveLimitID
}

// lunaReserveLimit returns the Reserve meter and its key, or nil.
func lunaReserveLimit(status any) (*obj, string) {
	byID := asObj(asObj(status).V("rateLimitsByLimitId"))
	keys := append([]string(nil), byID.Keys()...)
	sort.Strings(keys)
	for _, k := range keys {
		lim, ok := byID.V(k).(*obj)
		if k == DefaultLimitID || !ok {
			continue
		}
		if isLunaReserve(lim) {
			return lim, k
		}
	}
	return nil, ""
}

func lunaReserveView(status any) *limitView {
	lim, key := lunaReserveLimit(status)
	if lim == nil {
		return nil
	}
	v := newLimitView(lim, key)
	return &v
}

func lunaReserveJSON(status any, fullPct float64) *obj {
	v := lunaReserveView(status)
	if v == nil {
		return nil
	}
	p := limitJSON(*v, fullPct)
	p.Set("label", LunaReserveLabel)
	p.Set("model", LunaReserveModel)
	p.Set("available", !p.V("blocked").(bool))
	return p
}

// limitsByID returns every per-model-family limit, sorted by id.
func limitsByID(status any) []limitView {
	byID := asObj(asObj(status).V("rateLimitsByLimitId"))
	keys := append([]string(nil), byID.Keys()...)
	sort.Strings(keys)
	var out []limitView
	for _, k := range keys {
		if isObj(byID.V(k)) {
			out = append(out, newLimitView(byID.V(k), k))
		}
	}
	return out
}

func blockingSignals(limit, envelope any) []string {
	data, outer := asObj(limit), asObj(envelope)
	var reasons []string
	for _, src := range []*obj{data, outer} {
		if b, ok := src.V("ordinaryUsageAllowed").(bool); ok && !b {
			reasons = append(reasons, "ordinary usage not allowed")
			break
		}
	}
	for _, src := range []*obj{data, outer} {
		if b, ok := src.V("spendControlReached").(bool); ok && b {
			reasons = append(reasons, "spend control reached")
			break
		}
	}
	reached := data.V("rateLimitReachedType")
	if !truthy(reached) {
		reached = outer.V("rateLimitReachedType")
	}
	if truthy(reached) {
		reasons = append(reasons, "rate limit reached ("+pyStr(reached)+")")
	}
	return reasons
}

// blockState says whether a limit blocks usage and when that ends: the
// latest reset among the blocked windows.
type blockState struct {
	Blocked        bool
	Reasons        []string
	BlockedWindows []rlWindow
	ResetsAt       *float64
}

func computeBlockState(limit, envelope any, fullPct float64) blockState {
	windows := iterWindows(limit)
	var blockedWindows []rlWindow
	for _, w := range windows {
		if w.blocked(fullPct) {
			blockedWindows = append(blockedWindows, w)
		}
	}
	reasons := []string{}
	for _, w := range blockedWindows {
		r := fmt.Sprintf("%s window at %s%%", w.Label, pyG(*w.UsedPercent))
		if fullPct != DefaultFullPct {
			r += fmt.Sprintf(" (>= %s%%)", pyG(fullPct))
		}
		reasons = append(reasons, r)
	}
	reasons = append(reasons, blockingSignals(limit, envelope)...)
	if len(reasons) == 0 {
		return blockState{Reasons: []string{}}
	}
	var resets []float64
	for _, w := range blockedWindows {
		if w.ResetsAt != nil {
			resets = append(resets, *w.ResetsAt)
		}
	}
	if len(resets) == 0 {
		for _, w := range windows {
			if w.ResetsAt != nil {
				resets = append(resets, *w.ResetsAt)
			}
		}
	}
	st := blockState{Blocked: true, Reasons: reasons, BlockedWindows: blockedWindows}
	if len(resets) > 0 {
		m := resets[0]
		for _, r := range resets[1:] {
			if r > m {
				m = r
			}
		}
		st.ResetsAt = &m
	}
	return st
}

func longestWindow(limit any) *rlWindow {
	var best *rlWindow
	for _, w := range iterWindows(limit) {
		if w.DurationMins == nil {
			continue
		}
		if best == nil || *w.DurationMins > *best.DurationMins {
			w := w
			best = &w
		}
	}
	return best
}

// resetCredit is one "Full reset" grant. The identity fields stay raw JSON
// values, as the Python passes them through untyped.
type resetCredit struct {
	ID, Title, Description, Status, ResetType any
	GrantedAt, ExpiresAt                      *float64
}

func (c resetCredit) available() bool {
	s, ok := c.Status.(string)
	return ok && s == "available"
}

type resetCredits struct {
	AvailableCount int64
	Credits        []resetCredit
}

func (r resetCredits) availableCredits() []resetCredit {
	var out []resetCredit
	for _, c := range r.Credits {
		if c.available() {
			out = append(out, c)
		}
	}
	return out
}

func getResetCredits(status any) resetCredits {
	data := asObj(asObj(status).V("rateLimitResetCredits"))
	raw, _ := data.V("credits").([]any)
	var credits []resetCredit
	for _, x := range raw {
		c, ok := x.(*obj)
		if !ok {
			continue
		}
		credits = append(credits, resetCredit{
			ID: c.V("id"), Title: c.V("title"), Description: c.V("description"),
			Status: c.V("status"), ResetType: c.V("resetType"),
			GrantedAt: numPtr(c.V("grantedAt")), ExpiresAt: numPtr(c.V("expiresAt")),
		})
	}
	rc := resetCredits{Credits: credits}
	if count, ok := numeric(data.V("availableCount")); ok {
		rc.AvailableCount = int64(count)
	} else {
		rc.AvailableCount = int64(len(rc.availableCredits()))
	}
	return rc
}

func windowJSON(w rlWindow, fullPct float64) *obj {
	return newObj().
		Set("key", w.Key).
		Set("label", w.Label).
		Set("windowDurationMins", jsonNumber(w.DurationMins)).
		Set("usedPercent", jsonNumber(w.UsedPercent)).
		Set("resetsAt", jsonNumber(w.ResetsAt)).
		Set("blocked", w.blocked(fullPct))
}

func windowsJSON(ws []rlWindow, fullPct float64) []any {
	out := []any{}
	for _, w := range ws {
		out = append(out, windowJSON(w, fullPct))
	}
	return out
}

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func reasonsJSON(rs []string) []any {
	out := []any{}
	for _, r := range rs {
		out = append(out, r)
	}
	return out
}

func limitJSON(v limitView, fullPct float64) *obj {
	st := computeBlockState(v.Raw, nil, fullPct)
	return newObj().
		Set("limitId", strOrNil(v.LimitID)).
		Set("limitName", strOrNil(v.LimitName)).
		Set("normalModelSlug", strOrNil(v.NormalModelSlug)).
		Set("blocked", st.Blocked).
		Set("reasons", reasonsJSON(st.Reasons)).
		Set("resetsAt", jsonNumber(st.ResetsAt)).
		Set("windows", windowsJSON(v.Windows, fullPct))
}

func resetCreditsJSON(rc resetCredits) *obj {
	credits := []any{}
	for _, c := range rc.Credits {
		credits = append(credits, newObj().
			Set("id", c.ID).
			Set("title", c.Title).
			Set("resetType", c.ResetType).
			Set("status", c.Status).
			Set("grantedAt", jsonNumber(c.GrantedAt)).
			Set("expiresAt", jsonNumber(c.ExpiresAt)))
	}
	return newObj().Set("availableCount", pyInt(fmt.Sprint(rc.AvailableCount))).Set("credits", credits)
}

func signalsJSON(limit, envelope any) *obj {
	data, outer := asObj(limit), asObj(envelope)
	pick := func(k string) any {
		if data.Has(k) {
			return data.V(k)
		}
		return outer.V(k)
	}
	return newObj().
		Set("ordinaryUsageAllowed", pick("ordinaryUsageAllowed")).
		Set("spendControlReached", pick("spendControlReached")).
		Set("rateLimitReachedType", pick("rateLimitReachedType")).
		Set("individualLimit", pick("individualLimit")).
		Set("rateLimitUpsell", pick("rateLimitUpsell"))
}
