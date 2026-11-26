// Package render holds the human and panel renderers of agent-quotas: the
// per-account block layout of agy-status (a port of agy_status_render.py) and
// the combined agent-status report and JSON document (a port of
// agent_status_render.py). Both work on the providers' legacy JSON rows, so
// their output matches the old tools byte for byte.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options tune a human report.
type Options struct {
	// Columns is the terminal width; below 40 counts as 40.
	Columns int
	// Color turns ANSI styling on. Decide it with AgyColor or
	// AgentStatusColor, which apply the old tools' rules.
	Color bool
	// Absolute is auto, y or n: whether local reset times are shown.
	Absolute string
	// Now fixes the clock; zero means time.Now().
	Now time.Time
	// Location is the zone of absolute times; nil means time.Local.
	Location *time.Location
}

func (o Options) now() float64 {
	t := o.Now
	if t.IsZero() {
		t = time.Now()
	}
	return epochOf(t)
}

// epochOf is the Unix time in seconds, exact for whole seconds.
func epochOf(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9
}

func (o Options) loc() *time.Location {
	if o.Location != nil {
		return o.Location
	}
	return time.Local
}

func (o Options) absolute() string {
	if o.Absolute == "" {
		return "auto"
	}
	return o.Absolute
}

// AgyColor is agy_status_render.py's colour rule: only on a terminal,
// never with NO_COLOR set, and never when mode is "never" ("always" does not
// force colour into a pipe).
func AgyColor(mode string, stdoutIsTTY bool) bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return stdoutIsTTY && !noColor && mode != "never"
}

// AgentStatusColor is agent_status_render.py's colour rule: never with
// NO_COLOR set; otherwise "always", or "auto" on a terminal.
func AgentStatusColor(mode string, stdoutIsTTY bool) bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor && (mode == "always" || (mode == "auto" && stdoutIsTTY))
}

var (
	reModelsSuffix = regexp.MustCompile(`(?i)[\s\p{Z}]+models?$`)
	reWindowSuffix = regexp.MustCompile(`-(?:5h|weekly)$`)
	reFiveHour     = regexp.MustCompile(`(?i)5h|hour`)
	reLimitWords   = regexp.MustCompile(`(?i)\b(?:limit|remaining)\b`)
)

// BucketNames is the short display family and window of one agy bucket:
// Gemini, Claude/GPT, or the model group with backend chrome trimmed; 5h,
// weekly, or what the label says.
func BucketNames(bucketID, group, label string) (family, window string) {
	b := NewObject()
	b.Set("bucket_id", bucketID)
	b.Set("group", group)
	b.Set("label", label)
	return bucketNames(b)
}

func bucketNames(bucket *Object) (string, string) {
	if bucket.Has("_display_family") {
		return Clean(bucket.Get("_display_family")), Clean(bucket.Get("_display_window"))
	}
	identity := Clean(bucket.Get("bucket_id"))
	group := Clean(or(bucket.Get("group"), identity))
	label := Clean(bucket.Get("label"))
	var family string
	switch identity {
	case "gemini-5h", "gemini-weekly":
		family = "Gemini"
	case "3p-5h", "3p-weekly":
		family = "Claude/GPT"
	default:
		family = reModelsSuffix.ReplaceAllString(group, "")
		if family == identity {
			family = pyTitle(strings.ReplaceAll(reWindowSuffix.ReplaceAllString(identity, ""), "-", " "))
		}
	}
	var window string
	switch {
	case strings.HasSuffix(identity, "-5h"):
		window = "5h"
	case strings.HasSuffix(identity, "-weekly") || strings.Contains(strings.ToLower(label), "weekly"):
		window = "weekly"
	case reFiveHour.MatchString(label):
		window = "5h"
	default:
		window = pyStrip(reLimitWords.ReplaceAllString(label, ""))
		if window == "" {
			window = "quota"
		}
	}
	return family, window
}

var cacheAliases = map[string]string{
	"gemini-4-argon-weekly": "gemini-argon-wtu-weekly",
	"gemini-4-argon-5h":     "gemini-argon-5h",
}

func provenance(account *Object, now float64) string {
	if account.Has("_display_source") {
		return Clean(account.Get("_display_source"))
	}
	buckets := asList(account.Get("buckets"))
	if len(buckets) == 0 {
		if truthy(account.Get("error")) {
			return "quota unavailable"
		}
		return ""
	}
	sources := map[string]bool{}
	for _, b := range buckets {
		if s, ok := asObject(b).Get("source").(string); ok {
			sources[s] = true
		}
	}
	var parts []string
	if sources["live"] {
		parts = append(parts, "live")
	}
	if sources["cache"] {
		captureOf := cacheCaptures(account)
		var ages []float64
		undated := false
		for _, b := range buckets {
			bucket := asObject(b)
			if s, _ := bucket.Get("source").(string); s != "cache" {
				continue
			}
			if best, have := captureOf(bucket); have {
				ages = append(ages, math.Max(0, now-best))
			} else {
				undated = true
			}
		}
		stamp := "age unknown"
		if len(ages) > 0 {
			// The newest reading: a pool read long before the rest says
			// its own age on its line (accountModel).
			age := ages[0]
			for _, a := range ages[1:] {
				age = math.Min(age, a)
			}
			if age < 60 {
				stamp = "<1m"
			} else {
				stamp = Duration(age)
			}
		}
		part := "cache " + stamp
		if undated && len(ages) > 0 {
			part += ", some undated"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "source unknown"
	}
	return strings.Join(parts, " + ")
}

type windowView struct {
	Family, Window string
	UsedPercent    Num
	ResetsAt       any // Num or nil
	// Old is "read 2d 14h ago" for a window read over oldReadingS before
	// the account's newest reading; the report only, not the JSON.
	Old string
}

// oldReadingS is how much older than an account's newest reading a window's
// own reading must be to say its age on its line.
const oldReadingS = 3600

// cacheCaptures returns the captured_at of a cache bucket of account, as
// its statusline cache (_display_cache_file) has it, under its id or the
// statusline's alias for it, the later of the two.
func cacheCaptures(account *Object) func(bucket *Object) (float64, bool) {
	var cached *Object
	if path, ok := account.Get("_display_cache_file").(string); ok && account.Has("_display_cache_file") {
		if data, err := os.ReadFile(path); err == nil {
			if doc, err := Decode(data); err == nil {
				cached = asObject(asObject(doc).Get("buckets"))
			}
		}
	}
	return func(bucket *Object) (float64, bool) {
		var candidates []any
		if identity, ok := bucket.Get("bucket_id").(string); ok {
			candidates = append(candidates, cached.Get(identity))
			if alias, ok := cacheAliases[identity]; ok {
				candidates = append(candidates, cached.Get(alias))
			}
		}
		best, have := 0.0, false
		for _, c := range candidates {
			co := asObject(c)
			if co == nil {
				continue
			}
			if n, ok := asNumLoose(co.Get("captured_at")); ok && finite(n.F) && (!have || n.F > best) {
				best, have = n.F, true
			}
		}
		return best, have
	}
}

type noteView struct{ Severity, Text string }

type accountView struct {
	Account, Name, State, Tier, Source, Email string
	Error, Login                              any // string or nil
	Windows                                   []windowView
	Notes                                     []noteView
}

func (a accountView) object() *Object {
	o := NewObject()
	o.Set("account", a.Account)
	o.Set("name", a.Name)
	o.Set("state", a.State)
	o.Set("tier", a.Tier)
	o.Set("source", a.Source)
	o.Set("email", a.Email)
	o.Set("error", a.Error)
	o.Set("login", a.Login)
	windows := []any{}
	for _, w := range a.Windows {
		wo := NewObject()
		wo.Set("family", w.Family)
		wo.Set("window", w.Window)
		wo.Set("used_percent", w.UsedPercent)
		wo.Set("resets_at", w.ResetsAt)
		windows = append(windows, wo)
	}
	o.Set("windows", windows)
	notes := []any{}
	for _, n := range a.Notes {
		no := NewObject()
		no.Set("severity", n.Severity)
		no.Set("text", n.Text)
		notes = append(notes, no)
	}
	o.Set("notes", notes)
	return o
}

type windowKey struct {
	rank   int
	folded string
	slot   int
	name   string
}

func orderOf(w windowView) windowKey {
	rank := 3
	switch {
	case w.Family == "Gemini":
		rank = 0
	case strings.HasPrefix(strings.ToLower(w.Family), "gemini"):
		rank = 1
	case w.Family == "Claude/GPT":
		rank = 2
	}
	slot := 1
	if w.Window == "5h" {
		slot = 0
	}
	return windowKey{rank, casefold(w.Family), slot, w.Window}
}

func (a windowKey) less(b windowKey) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	if a.folded != b.folded {
		return a.folded < b.folded
	}
	if a.slot != b.slot {
		return a.slot < b.slot
	}
	return a.name < b.name
}

// badReport is what Python raised (TypeError, KeyError, ...) on a malformed
// row; it costs the section, not the report.
type badReport struct{ msg string }

func (b badReport) Error() string { return b.msg }

func bad(format string, args ...any) { panic(badReport{fmt.Sprintf(format, args...)}) }

// clampUsed is max(0, min(100, float(used))), including Python returning
// the int bound itself (so JSON says 100, not 100.0).
func clampUsed(used any) Num {
	f, ok := pyFloat(used)
	if !ok {
		bad("float() argument must be a string or a real number, not '%s'", pyTypeName(used))
	}
	if !(f < 100) && !math.IsNaN(f) {
		return Int(100)
	}
	if !(f > 0) && !math.IsNaN(f) {
		return Int(0)
	}
	return Float(f)
}

func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case *Object:
		return "dict"
	case []any:
		return "list"
	case string:
		return "str"
	case bool:
		return "bool"
	}
	return "float"
}

func accountModel(account *Object, now float64) accountView {
	var windows []windowView
	captureOf := cacheCaptures(account)
	var captures []float64 // per window; -1 when unknown
	newest := -1.0
	for _, b := range asList(account.Get("buckets")) {
		bucket := asObject(b)
		if bucket == nil {
			bad("'%s' object has no attribute 'get'", pyTypeName(b))
		}
		family, window := bucketNames(bucket)
		// bucket.get("used_percent", 100 - bucket.get("remaining_percent", 100))
		var dflt any = Int(100)
		if bucket.Has("remaining_percent") {
			r, ok := asNumLoose(bucket.Get("remaining_percent"))
			if !ok {
				bad("unsupported operand type(s) for -: 'int' and '%s'", pyTypeName(bucket.Get("remaining_percent")))
			}
			if r.IsInt {
				dflt = Num{F: 100 - r.F, IsInt: true}
			} else {
				dflt = Float(100 - r.F)
			}
		}
		used := dflt
		if bucket.Has("used_percent") {
			used = bucket.Get("used_percent")
		}
		reset := bucket.Get("resets_at")
		var resets any
		if n, ok := asNumLoose(reset); ok && finite(n.F) && n.F > 0 {
			resets = reset
		}
		captured := -1.0
		switch src, _ := bucket.Get("source").(string); src {
		case "live":
			captured = now
		case "cache":
			if c, ok := captureOf(bucket); ok {
				captured = c
			}
		}
		newest = math.Max(newest, captured)
		captures = append(captures, captured)
		windows = append(windows, windowView{family, window, clampUsed(used), resets, ""})
	}
	for i, c := range captures {
		if c >= 0 && newest-c > oldReadingS {
			windows[i].Old = "read " + Duration(math.Max(0, now-c)) + " ago"
		}
	}
	sort.SliceStable(windows, func(i, j int) bool { return orderOf(windows[i]).less(orderOf(windows[j])) })
	label := Clean(or(account.Get("label"), account.Get("account")))
	signed := account.Get("signed_in")
	state := "auth stored"
	if v, ok := signed.(bool); ok {
		if v {
			state = "signed in"
		} else {
			state = "not signed in"
		}
	}
	name := label
	if raw, ok := account.Get("account").(string); !ok || label != raw {
		name = label + " [" + Clean(account.Get("account")) + "]"
	}
	view := accountView{
		Account: Clean(account.Get("account")),
		Name:    name,
		State:   Clean(or(account.Get("_display_state"), state)),
		Tier:    Clean(account.Get("tier")),
		Source:  provenance(account, now),
		Email:   Clean(account.Get("email")),
		Windows: windows,
	}
	if e := Clean(account.Get("error")); e != "" {
		view.Error = e
	}
	if v, ok := signed.(bool); ok && !v {
		if !account.Has("account") {
			bad("'account'")
		}
		view.Login = "agy-acc-login " + pyStr(account.Get("account"))
	}
	for _, n := range asList(account.Get("_display_notes")) {
		pair := asList(n)
		if len(pair) != 2 {
			bad("bad note")
		}
		sev := pyStr(pair[0])
		if sev == "dim" {
			sev = "info"
		}
		view.Notes = append(view.Notes, noteView{sev, Clean(pair[1])})
	}
	return view
}

type entry struct {
	family, window string
	used           float64
	relative       string
	stamp          string
	old            string
}

func renderAccounts(accounts []*Object, columns int, color bool, absolute string, now float64, loc *time.Location) string {
	if columns < 40 {
		columns = 40
	}
	off, dim, bold := "", "", ""
	if color {
		off, dim, bold = "\033[0m", "\033[2m", "\033[1m"
	}
	paint := func(text string, code int) string {
		if color {
			return fmt.Sprintf("\033[%dm%s%s", code, text, off)
		}
		return text
	}
	var rows []entry
	type prepared struct {
		view    accountView
		entries []entry
	}
	var preps []prepared
	for _, a := range accounts {
		view := accountModel(a, now)
		var entries []entry
		for _, w := range view.Windows {
			rel, stamp := resetText(w.ResetsAt, now, loc)
			entries = append(entries, entry{w.Family, w.Window, w.UsedPercent.F, rel, stamp, w.Old})
		}
		rows = append(rows, entries...)
		preps = append(preps, prepared{view, entries})
	}
	maxOf := func(floor int, f func(entry) string) int {
		m := floor
		for _, r := range rows {
			if w := Width(f(r)); w > m {
				m = w
			}
		}
		return m
	}
	windowWidth := min(8, maxOf(6, func(r entry) string { return r.window }))
	resetWidth := min(23, maxOf(12, func(r entry) string { return r.relative }))
	barWidth := 6
	if columns >= 70 {
		barWidth = 10
	}
	fixedWidth := 2 + 2 + windowWidth + 2 + barWidth + 2 + 5 + 2 + resetWidth
	familyWidth := min(maxOf(10, func(r entry) string { return r.family }), max(8, columns-fixedWidth))
	baseWidth := fixedWidth + familyWidth
	stampWidth := maxOf(0, func(r entry) string { return r.stamp })
	showAbsolute := absolute != "n" && baseWidth+3+stampWidth <= columns
	if absolute == "auto" {
		showAbsolute = showAbsolute && columns >= 100
	}

	headers := make([][5]string, len(preps))
	for i, p := range preps {
		headers[i] = [5]string{p.view.Name, p.view.State, p.view.Tier, p.view.Source, p.view.Email}
	}
	var widths [4]int
	for i := 0; i < 4; i++ {
		for _, h := range headers {
			widths[i] = max(widths[i], Width(h[i]))
		}
	}
	limits := [4]int{max(8, columns/4), 13, 24, 32}
	minimums := [4]int{8, 13, 8, 12}
	var minima [4]int
	for i := range widths {
		widths[i] = min(widths[i], limits[i])
		minima[i] = min(widths[i], minimums[i])
	}
	emailReserve := 0
	for _, h := range headers {
		if h[4] != "" {
			emailReserve = 14
		}
	}
	sum := func() int { return widths[0] + widths[1] + widths[2] + widths[3] }
	for sum()+6+emailReserve > columns {
		widest := -1
		for _, i := range []int{0, 2, 3} {
			if widths[i] > minima[i] && (widest < 0 || widths[i] > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		widths[widest]--
	}
	emailWidth := max(0, columns-sum()-8)

	var out []string
	for i, p := range preps {
		if len(out) > 0 {
			out = append(out, "")
		}
		fields := headers[i]
		cells := make([]string, 4)
		for j := 0; j < 4; j++ {
			value := Clip(fields[j], widths[j])
			style := ""
			if j == 0 {
				style = bold
			} else if j == 3 {
				style = dim
			}
			cell := style + value
			if style != "" {
				cell += off
			}
			cells[j] = cell + strings.Repeat(" ", max(0, widths[j]-Width(value)))
		}
		header := strings.Join(cells, "  ")
		if fields[4] != "" && emailWidth != 0 {
			header += "  " + dim + Clip(fields[4], emailWidth) + off
		}
		out = append(out, rstrip(header))
		if e, ok := p.view.Error.(string); ok {
			out = append(out, paint("  ! "+Clip(e, columns-4), 31))
		}
		if l, ok := p.view.Login.(string); ok {
			out = append(out, "  "+Clip(l, columns-2))
		}
		for _, e := range p.entries {
			level := 32
			if e.used >= 90 {
				level = 31
			} else if e.used >= 70 {
				level = 33
			}
			filled := min(barWidth, int(e.used*float64(barWidth)/100+0.5))
			bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
			percent := strconv.FormatFloat(e.used, 'f', 1, 64)
			percent = strings.TrimRight(strings.TrimRight(percent, "0"), ".") + "%"
			line := "  " + Pad(e.family, familyWidth, false) + "  " + Pad(e.window, windowWidth, false) + "  " +
				paint(bar, level) + "  " + paint(Pad(percent, 5, true), level) + "  " + Pad(e.relative, resetWidth, false)
			if showAbsolute && e.stamp != "" {
				line += "   " + dim + e.stamp + off
			}
			if e.old != "" {
				line += "   " + dim + e.old + off
			}
			out = append(out, rstrip(line))
		}
		for _, n := range p.view.Notes {
			code := 2
			switch n.Severity {
			case "alarm":
				code = 31
			case "warning":
				code = 33
			}
			out = append(out, paint("  "+Clip(n.Text, columns-2), code))
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// RenderAgy writes the human agy-status report for the account objects of
// agy-status --json. cacheFiles maps an account name to its statusline cache,
// which the header reads for the cache age (never shown, never written).
func RenderAgy(w io.Writer, rows []json.RawMessage, cacheFiles map[string]string, opts Options) error {
	accounts, err := agyRows(rows, cacheFiles)
	if err != nil {
		return err
	}
	text, err := safeRender(func() string {
		return renderAccounts(accounts, opts.Columns, opts.Color, opts.absolute(), opts.now(), opts.loc())
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, text)
	return err
}

func agyRows(rows []json.RawMessage, cacheFiles map[string]string) ([]*Object, error) {
	var accounts []*Object
	for _, raw := range rows {
		v, err := Decode(raw)
		if err != nil {
			return nil, err
		}
		o := asObject(v)
		if o == nil {
			return nil, fmt.Errorf("agy account row is not an object")
		}
		o = o.Copy()
		if name, ok := o.Get("account").(string); ok {
			if path, ok := cacheFiles[name]; ok {
				o.Set("_display_cache_file", path)
			}
		}
		accounts = append(accounts, o)
	}
	return accounts, nil
}

func safeRender(f func() string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			b, ok := r.(badReport)
			if !ok {
				panic(r)
			}
			err = b
		}
	}()
	return f(), nil
}
