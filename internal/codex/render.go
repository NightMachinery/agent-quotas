package codex

// The human report of codex_status.py (print_human_statuses) and the colour
// layer of common_sub_status.py.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// RGB is one true-colour value.
type RGB [3]int

// Theme is common_sub_status.ColorTheme.
type Theme struct {
	Heading, Identity, OK, Warn, Error, Dim, ResetTime RGB
}

// DarkThemes and LightThemes are the named true-colour themes.
var (
	DarkThemes = map[string]Theme{
		"neon":  {RGB{80, 220, 255}, RGB{255, 110, 210}, RGB{80, 220, 135}, RGB{245, 195, 80}, RGB{255, 95, 95}, RGB{135, 145, 160}, RGB{125, 185, 255}},
		"ember": {RGB{255, 175, 95}, RGB{230, 130, 255}, RGB{110, 220, 140}, RGB{245, 210, 95}, RGB{255, 105, 115}, RGB{145, 135, 125}, RGB{255, 145, 95}},
		"ocean": {RGB{95, 200, 230}, RGB{145, 165, 255}, RGB{85, 210, 175}, RGB{235, 200, 90}, RGB{255, 100, 120}, RGB{130, 150, 165}, RGB{80, 185, 255}},
	}
	LightThemes = map[string]Theme{
		"day":   {RGB{0, 110, 150}, RGB{165, 55, 145}, RGB{20, 130, 75}, RGB{165, 105, 0}, RGB{195, 45, 55}, RGB{105, 110, 120}, RGB{20, 105, 190}},
		"paper": {RGB{20, 95, 125}, RGB{140, 65, 120}, RGB{40, 125, 80}, RGB{150, 105, 25}, RGB{180, 55, 60}, RGB{115, 105, 95}, RGB{30, 95, 165}},
		"mint":  {RGB{0, 120, 115}, RGB{150, 65, 150}, RGB{20, 135, 90}, RGB{155, 115, 15}, RGB{185, 55, 70}, RGB{100, 115, 110}, RGB{0, 115, 170}},
	}
)

// Default theme names.
const (
	DarkThemeDefault  = "neon"
	LightThemeDefault = "day"
)

// style is common_sub_status.Style.
type style struct {
	enabled   bool
	trueColor bool
	theme     *Theme
}

var plainStyle = style{}

func (s style) wrap(text, code string) string {
	if !s.enabled {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

func (s style) rgb(text string, c RGB, fallback string) string {
	if !s.enabled {
		return text
	}
	if s.trueColor {
		return s.wrap(text, fmt.Sprintf("38;2;%d;%d;%d", c[0], c[1], c[2]))
	}
	return s.wrap(text, fallback)
}

func (s style) bold(t string) string    { return s.wrap(t, "1") }
func (s style) inverse(t string) string { return s.wrap(t, "7") }

func (s style) themed(t string, pick func(Theme) RGB, code string) string {
	if s.theme == nil {
		return s.wrap(t, code)
	}
	return s.rgb(t, pick(*s.theme), code)
}

func (s style) cyan(t string) string {
	return s.themed(t, func(th Theme) RGB { return th.Heading }, "36")
}
func (s style) magenta(t string) string {
	return s.themed(t, func(th Theme) RGB { return th.Identity }, "35")
}
func (s style) green(t string) string { return s.themed(t, func(th Theme) RGB { return th.OK }, "32") }
func (s style) yellow(t string) string {
	return s.themed(t, func(th Theme) RGB { return th.Warn }, "33")
}
func (s style) red(t string) string { return s.themed(t, func(th Theme) RGB { return th.Error }, "31") }
func (s style) dim(t string) string {
	if s.theme == nil || !s.trueColor {
		return s.wrap(t, "2")
	}
	return s.rgb(t, s.theme.Dim, "2")
}
func (s style) resetTime(t string) string {
	if s.theme == nil {
		return s.cyan(t)
	}
	return s.rgb(t, s.theme.ResetTime, "36")
}

// RenderOptions configure RenderHuman.
type RenderOptions struct {
	// Color turns ANSI colour on.
	Color bool
	// TrueColor uses the RGB theme; without it colours are the basic eight.
	TrueColor bool
	// Light picks LightTheme instead of DarkTheme (true colour only).
	Light bool
	// DarkTheme and LightTheme name the themes; the defaults when empty.
	DarkTheme, LightTheme string
	// Width is accepted for symmetry with the other renderers; the Codex
	// report does not wrap.
	Width int
	// Now is the clock for relative times; time.Now when zero.
	Now time.Time
	// Location is the zone of absolute timestamps; time.Local when nil.
	Location *time.Location
	// All is --all: per-auth headings with [Active] tags and the average
	// block. Without it the report is the single "Codex rate limits" block.
	All bool
	// ActivePath is the live auth.json; a row whose file has the same bytes
	// is tagged [Active] and printed last. Empty means none is active.
	ActivePath string
	// Home expands "~/" in rows' authFile; config.Home() when empty.
	Home string
	// FullPct is the spent threshold; DefaultFullPct when zero.
	FullPct float64
}

func (o RenderOptions) style() style {
	if !o.Color {
		return style{}
	}
	st := style{enabled: true, trueColor: o.TrueColor}
	if o.TrueColor {
		var th Theme
		var ok bool
		if o.Light {
			th, ok = LightThemes[o.LightTheme]
			if !ok {
				th = LightThemes[LightThemeDefault]
			}
		} else {
			th, ok = DarkThemes[o.DarkTheme]
			if !ok {
				th = DarkThemes[DarkThemeDefault]
			}
		}
		st.theme = &th
	}
	return st
}

type renderer struct {
	w    *bytes.Buffer
	st   style
	now  float64
	loc  *time.Location
	d    display
	opts RenderOptions
}

func (r *renderer) println(s string) { r.w.WriteString(s + "\n") }

// relativeParts is common_sub_status.relative_parts.
func (r *renderer) relativeParts(epoch float64) (string, bool) {
	delta := int64(math.RoundToEven(epoch - r.now))
	past := delta < 0
	if past {
		delta = -delta
	}
	weeks, rem := delta/(7*86400), delta%(7*86400)
	days, rem := rem/86400, rem%86400
	hours, rem := rem/3600, rem%3600
	mins, secs := rem/60, rem%60
	var parts []string
	if weeks > 0 {
		parts = append(parts, fmt.Sprintf("%dw", weeks))
	}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if secs > 0 && len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	if len(parts) == 0 {
		parts = append(parts, "0s")
	}
	return strings.Join(parts, " "), past
}

func (r *renderer) formatRelative(epoch *float64) string {
	if epoch == nil {
		return "n/a"
	}
	text, past := r.relativeParts(*epoch)
	if past {
		return text + " ago"
	}
	return "in " + text
}

func (r *renderer) formatRelativeColored(epoch *float64) string {
	if epoch == nil {
		return r.st.dim("n/a")
	}
	text, past := r.relativeParts(*epoch)
	c := r.st.resetTime(text)
	if past {
		return c + " ago"
	}
	return "in " + c
}

func (r *renderer) formatTimestamp(epoch float64) string {
	return model.Time(epoch).In(r.loc).Format("2006-01-02 15:04:05 -0700")
}

func formatUsedPercent(st style, used *float64) string {
	if used == nil {
		return "n/a"
	}
	pct := *used
	text := pyG(pct) + "%"
	switch {
	case pct >= 90:
		return st.red(text)
	case pct >= 75:
		return st.yellow(text)
	}
	return st.green(text)
}

func formatAveragePercent(st style, used *float64) string {
	if used == nil {
		return st.dim("n/a")
	}
	v := pyRound1(*used)
	return formatUsedPercent(st, &v)
}

func (r *renderer) formatWindowLine(w rlWindow) string {
	bits := []string{w.Label + ": " + formatUsedPercent(r.st, w.UsedPercent) + " used"}
	if w.ResetsAt == nil {
		bits = append(bits, "resets "+r.st.dim("n/a"))
	} else {
		bits = append(bits, "resets "+r.formatRelative(w.ResetsAt)+" ("+r.formatTimestamp(*w.ResetsAt)+")")
	}
	if w.DurationMins != nil {
		bits = append(bits, "window "+pyG(*w.DurationMins)+"m")
	}
	return strings.Join(bits, " | ")
}

func (r *renderer) formatLimitLine(v limitView) string {
	var parts []string
	for _, w := range v.Windows {
		s := w.Label + " " + formatUsedPercent(r.st, w.UsedPercent)
		if w.ResetsAt != nil {
			s += " (resets " + r.formatRelative(w.ResetsAt) + ")"
		}
		parts = append(parts, s)
	}
	windows := strings.Join(parts, " | ")
	if windows == "" {
		windows = r.st.dim("no windows reported")
	}
	return r.st.magenta(v.displayName()) + ": " + windows
}

func (r *renderer) formatLunaReserveLine(v limitView) string {
	state := computeBlockState(v.Raw, nil, r.d.FullPct)
	verdict := ""
	if state.Blocked {
		verdict = " (" + r.st.red("spent") + ")"
	}
	var parts []string
	for _, w := range v.Windows {
		parts = append(parts, r.formatWindowLine(w))
	}
	windows := strings.Join(parts, " | ")
	if windows == "" {
		windows = r.st.dim("no windows reported")
	}
	return r.st.magenta(LunaReserveLabel) + " [" + LunaReserveModel + "]" + verdict + ": " + windows
}

func (r *renderer) formatExpiry(expires float64) string {
	daysLeft := math.Floor((expires - r.now) / 86400)
	text := r.formatRelative(&expires)
	switch {
	case daysLeft <= 3:
		return r.st.inverse(r.st.red(r.st.bold(text)))
	case daysLeft <= 7:
		return r.st.red(r.st.bold(text))
	case daysLeft <= 14:
		return r.st.yellow(r.st.bold(text))
	}
	return text
}

func (r *renderer) formatResetCredits(rc resetCredits) string {
	var titles []string
	seen := map[string]bool{}
	var expiry []float64
	for _, c := range rc.availableCredits() {
		if truthy(c.Title) {
			t := pyStr(c.Title)
			if !seen[t] {
				seen[t] = true
				titles = append(titles, t)
			}
		}
		if c.ExpiresAt != nil {
			expiry = append(expiry, *c.ExpiresAt)
		}
	}
	text := r.st.green(fmt.Sprintf("%d available", rc.AvailableCount))
	if len(titles) > 0 {
		text += " (" + strings.Join(titles, ", ")
		if len(expiry) > 0 {
			m := expiry[0]
			for _, e := range expiry[1:] {
				if e < m {
					m = e
				}
			}
			text += ", expires " + r.formatExpiry(m)
		}
		text += ")"
	}
	return text
}

// formatCredits is format_credits; "" when there are no credits to report.
func formatCredits(st style, credits *obj) string {
	if credits == nil {
		return ""
	}
	var base string
	switch {
	case truthy(credits.V("unlimited")):
		base = st.green("unlimited")
	case truthy(credits.V("hasCredits")):
		base = st.green("available")
	default:
		return ""
	}
	if b := credits.V("balance"); !emptyish(b) {
		return base + " (balance: " + pyStr(b) + ")"
	}
	return base
}

func (r *renderer) printRateDetails(s authStatus) {
	plan := "unknown"
	if v := s.Identity.V("planType"); truthy(v) {
		plan = pyStr(v)
	}
	email := s.Identity.V("planOwnerEmail")
	workspace := s.Identity.V("workspaceName")
	lim, _ := s.codexLimit()
	credits := asObj(lim.V("credits"))

	if plan != "unknown" {
		r.println("Plan: " + r.st.magenta(plan))
	} else {
		r.println("Plan: " + plan)
	}
	if truthy(email) {
		r.println("Plan owner: " + pyStr(email))
	} else {
		r.println("Plan owner: " + r.st.dim("n/a"))
	}
	if truthy(workspace) {
		r.println("Workspace: " + r.st.magenta(pyStr(workspace)))
	} else {
		r.println("Workspace: " + r.st.dim("n/a"))
	}
	if windows := iterWindows(lim); len(windows) > 0 {
		for _, w := range windows {
			r.println(r.formatWindowLine(w))
		}
	} else {
		r.println("Windows: " + r.st.dim("none reported"))
	}
	if line := formatCredits(r.st, credits); line != "" {
		r.println("Credits: " + line)
	}
	reserve := lunaReserveView(s.rateResult())
	if reserve != nil {
		r.println(r.formatLunaReserveLine(*reserve))
	}
	if rc := getResetCredits(s.rateResult()); rc.AvailableCount > 0 {
		r.println("Reset credits: " + r.formatResetCredits(rc))
	}
	if state := s.blockState(r.d); state.Blocked {
		note := ""
		if reserve != nil && !computeBlockState(reserve.Raw, nil, r.d.FullPct).Blocked {
			note = r.st.green(" (" + LunaReserveLabel + " still available)")
		}
		r.println("Blocked: " + r.st.red(strings.Join(state.Reasons, "; ")) + note)
	}
	for _, v := range s.reportableLimits(r.d) {
		r.println(r.formatLimitLine(v))
	}
}

func (r *renderer) isActive(s authStatus) bool {
	if r.opts.ActivePath == "" || !isRegular(r.opts.ActivePath) {
		return false
	}
	for _, p := range s.authPaths() {
		if authBytesEqual(r.opts.ActivePath, p) {
			return true
		}
	}
	return false
}

func (r *renderer) printStatus(s authStatus, showHeader bool) {
	title := "Codex rate limits"
	if showHeader && s.Source.DisplayPath != "" {
		title = "* " + s.Source.DisplayPath
		if r.isActive(s) {
			title += " " + r.st.magenta("[Active]")
		}
	}
	r.println(r.st.bold(r.st.cyan(title)))
	if !s.OK {
		r.println("Status: " + r.st.red("failed"))
		msg := s.Error
		if msg == "" {
			msg = "unknown error"
		}
		r.println("Error: " + msg)
		if e := pyStrip(s.Stderr); e != "" {
			r.println(r.st.dim(e))
		}
		return
	}
	r.printRateDetails(s)
	if s.RecoveredFrom != nil && s.RecoveredFrom.DisplayPath != "" {
		r.println("Recovered auth: " + r.st.dim(s.RecoveredFrom.DisplayPath))
	}
}

func (r *renderer) printAverage(statuses []authStatus) {
	rows := averageWindowsByDuration(statuses)
	first := firstQuotaReset(statuses, r.d)
	r.println(r.st.bold(r.st.cyan("Average usage")))
	for _, row := range rows {
		u := row.UsedPercent
		r.println(row.Label + ": " + formatAveragePercent(r.st, &u) + " used")
	}
	if len(rows) == 0 {
		r.println("Windows: " + r.st.dim("none reported"))
	}
	usable, total := 0, 0
	for _, s := range statuses {
		if s.OK {
			total++
			if s.usable(r.d) {
				usable++
			}
		}
	}
	r.println(fmt.Sprintf("Usable auths: %d/%d", usable, total))
	if first != nil {
		alias := r.st.dim("n/a")
		if first.Status.Source.Label != "" {
			alias = r.st.magenta(first.Status.Source.Label)
		}
		rt := first.ResetTime
		r.println("First Time to Reset: " + alias + " " + r.formatRelativeColored(&rt) + " (" + r.formatTimestamp(rt) + ")")
	}
}

// RenderHuman writes codex_status.py's human report for rows (each one
// authFiles entry, e.g. Snapshot.Native): one block per auth, the active one
// last, then (with All and more than one auth) the average block.
func RenderHuman(w io.Writer, rows []json.RawMessage, opts RenderOptions) error {
	statuses, err := statusesFromRows(rows, opts.Home)
	if err != nil {
		return err
	}
	r := &renderer{w: &bytes.Buffer{}, st: opts.style(), loc: opts.Location, opts: opts, d: display{FullPct: opts.FullPct}}
	if r.d.FullPct == 0 {
		r.d.FullPct = DefaultFullPct
	}
	if r.loc == nil {
		r.loc = time.Local
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	r.now = model.Unix(now)

	// Active last, stable otherwise (sorted by the bool, as the Python).
	var inactive, active []authStatus
	for _, s := range statuses {
		if r.isActive(s) {
			active = append(active, s)
		} else {
			inactive = append(inactive, s)
		}
	}
	for i, s := range append(inactive, active...) {
		if i > 0 {
			r.println("")
		}
		r.printStatus(s, opts.All)
	}
	if opts.All && len(statuses) > 1 {
		r.println("")
		r.printAverage(statuses)
	}
	_, err = w.Write(r.w.Bytes())
	return err
}
