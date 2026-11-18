package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// RenderOptions tune the human report.
type RenderOptions struct {
	// Color turns on ANSI colour (the old --color always).
	Color bool
	// TrueColor uses the 24-bit default theme (neon on dark, day on light)
	// instead of the basic 8-colour codes.
	TrueColor bool
	// Light picks the light theme with TrueColor.
	Light bool
	// Width is the terminal width; 0 means 100. Never below 40.
	Width int
	// Now is the clock; its Location is the display zone. Zero means now.
	Now time.Time
}

type rgb [3]int

type theme struct{ heading, identity, ok, warn, err, dim, resetTime rgb }

var (
	themeNeon = theme{rgb{80, 220, 255}, rgb{255, 110, 210}, rgb{80, 220, 135}, rgb{245, 195, 80},
		rgb{255, 95, 95}, rgb{135, 145, 160}, rgb{125, 185, 255}}
	themeDay = theme{rgb{0, 110, 150}, rgb{165, 55, 145}, rgb{20, 130, 75}, rgb{165, 105, 0},
		rgb{195, 45, 55}, rgb{105, 110, 120}, rgb{20, 105, 190}}
)

// style is common_sub_status.Style.
type style struct {
	enabled   bool
	trueColor bool
	theme     *theme
}

func newStyle(o RenderOptions) style {
	s := style{enabled: o.Color}
	if o.Color && o.TrueColor {
		s.trueColor = true
		t := themeNeon
		if o.Light {
			t = themeDay
		}
		s.theme = &t
	}
	return s
}

func (s style) wrap(text, code string) string {
	if !s.enabled {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

func (s style) rgb(text string, c rgb, fallback string) string {
	if s.theme == nil || !s.trueColor {
		return s.wrap(text, fallback)
	}
	return s.wrap(text, fmt.Sprintf("38;2;%d;%d;%d", c[0], c[1], c[2]))
}

func (s style) bold(t string) string { return s.wrap(t, "1") }
func (s style) cyan(t string) string {
	return s.rgbOr(t, func(th *theme) rgb { return th.heading }, "36")
}
func (s style) magenta(t string) string {
	return s.rgbOr(t, func(th *theme) rgb { return th.identity }, "35")
}
func (s style) green(t string) string { return s.rgbOr(t, func(th *theme) rgb { return th.ok }, "32") }
func (s style) yellow(t string) string {
	return s.rgbOr(t, func(th *theme) rgb { return th.warn }, "33")
}
func (s style) red(t string) string { return s.rgbOr(t, func(th *theme) rgb { return th.err }, "31") }
func (s style) dim(t string) string { return s.rgbOr(t, func(th *theme) rgb { return th.dim }, "2") }

func (s style) rgbOr(t string, pick func(*theme) rgb, code string) string {
	if s.theme == nil {
		return s.wrap(t, code)
	}
	return s.rgb(t, pick(s.theme), code)
}

// renderRow is what the renderer reads from a legacy row.
type renderRow struct {
	Profile        *string  `json:"profile"`
	Error          *string  `json:"error"`
	Source         string   `json:"source"`
	Warnings       []string `json:"warnings"`
	Plan           *string  `json:"plan"`
	TokenSource    *string  `json:"token_source"`
	TokenExpired   *bool    `json:"token_expired"`
	TokenExpiresAt *float64 `json:"token_expires_at"`
	Relogin        *string  `json:"relogin"`
	Cache          struct {
		FromCache   bool     `json:"from_cache"`
		StaleReason *string  `json:"stale_reason"`
		FetchedAt   *float64 `json:"fetched_at"`
	} `json:"cache"`
	Raw json.RawMessage `json:"raw"`

	payload *value
}

func decodeRow(raw json.RawMessage) (*renderRow, error) {
	var r renderRow
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Error == nil {
		p, err := parseValue(r.Raw)
		if err != nil || !p.isObject() {
			p = &value{kind: 'o', obj: map[string]*value{}}
		}
		r.payload = p
	}
	return &r, nil
}

type renderCtx struct {
	st    style
	now   float64
	loc   *time.Location
	width int
}

func newRenderCtx(o RenderOptions) renderCtx {
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	w := o.Width
	if w <= 0 {
		w = 100
	}
	if w < 40 {
		w = 40
	}
	return renderCtx{st: newStyle(o), now: model.Unix(now), loc: now.Location(), width: w}
}

// RenderHuman writes the human report of the rows, profiles separated by a
// blank line, exactly as claude_code_usage.py printed it.
func RenderHuman(w io.Writer, rows []json.RawMessage, opts RenderOptions) error {
	rc := newRenderCtx(opts)
	parts := make([]string, 0, len(rows))
	for _, raw := range rows {
		r, err := decodeRow(raw)
		if err != nil {
			return err
		}
		parts = append(parts, rc.human(r))
	}
	_, err := io.WriteString(w, strings.Join(parts, "\n\n")+"\n")
	return err
}

// RenderCloudCredit writes the --cloud-credit report: each profile's credit
// line, spent or expired ones included, or its error; "No cloud credit."
// when no profile has one.
func RenderCloudCredit(w io.Writer, rows []json.RawMessage, opts RenderOptions) error {
	rc := newRenderCtx(opts)
	var lines []string
	for _, raw := range rows {
		r, err := decodeRow(raw)
		if err != nil {
			return err
		}
		if line := rc.cloudOnly(r); line != "" {
			lines = append(lines, line)
		}
	}
	out := rc.st.dim("No cloud credit.")
	if len(lines) > 0 {
		out = strings.Join(lines, "\n")
	}
	_, err := io.WriteString(w, out+"\n")
	return err
}

func label(r *renderRow) string {
	if r.Profile == nil {
		return ""
	}
	return *r.Profile
}

func (rc renderCtx) cloudOnly(r *renderRow) string {
	st := rc.st
	prefix := ""
	if l := label(r); l != "" {
		prefix = "[" + l + "] "
	}
	if r.Error != nil {
		return prefix + st.red(*r.Error)
	}
	line := rc.cloudCredit(extractCloudCredit(r.payload, rc.now, rc.loc), true)
	if line == "" {
		return ""
	}
	if r.Cache.StaleReason != nil {
		line += " " + st.yellow("[stale: "+*r.Cache.StaleReason+"]")
	}
	return prefix + line
}

func (rc renderCtx) cloudCredit(c *cloudCredit, always bool) string {
	if c == nil || !(always || c.Available) {
		return ""
	}
	st := rc.st
	text := fmt.Sprintf("%s: $%s of $%s left", st.bold("Cloud credit"), pyMoney(c.RemainingDollars), pyMoney(c.LimitDollars))
	if c.ExpiresAt == nil {
		text += ", " + st.dim("expiry unknown")
	} else {
		verb := "expires"
		if *c.ExpiresAt <= rc.now {
			verb = "expired"
		}
		text += fmt.Sprintf(", %s %s (%s)", verb, formatRelative(*c.ExpiresAt, rc.now), formatTimestamp(*c.ExpiresAt, rc.loc))
	}
	if c.LockedReason != nil {
		text += " " + st.red("[locked: "+*c.LockedReason+"]")
	}
	return text
}

func (rc renderCtx) usedPercent(pct float64) string {
	text := pyG(pct) + "%"
	switch {
	case pct >= 90:
		return rc.st.red(text)
	case pct >= 75:
		return rc.st.yellow(text)
	}
	return rc.st.green(text)
}

func (rc renderCtx) window(w usageWindow) string {
	st := rc.st
	used := st.dim("n/a")
	if w.pct != nil {
		used = rc.usedPercent(pyRound1(*w.pct))
	}
	bits := []string{fmt.Sprintf("%s: %s used", st.bold(w.label), used)}
	if w.resetsAt == nil {
		bits = append(bits, "resets "+st.dim("n/a"))
	} else {
		bits = append(bits, fmt.Sprintf("resets %s (%s)", formatRelative(*w.resetsAt, rc.now), formatTimestamp(*w.resetsAt, rc.loc)))
		if *w.resetsAt < rc.now {
			// Only reachable with cached data; the percent is kept, not
			// rewritten to 0.
			bits = append(bits, st.dim("(rolled over)"))
		}
	}
	if w.severity != nil && *w.severity != "normal" {
		bits = append(bits, st.red(*w.severity))
	}
	return strings.Join(bits, " | ")
}

func (rc renderCtx) human(r *renderRow) string {
	st := rc.st
	heading := "Claude Code plan usage"
	if l := label(r); l != "" {
		heading += " [" + l + "]"
	}
	lines := []string{st.bold(st.cyan(heading))}
	if r.Error != nil {
		lines = append(lines, st.red(*r.Error))
		return strings.Join(lines, "\n")
	}

	plan := "unknown"
	if r.Plan != nil && *r.Plan != "" {
		plan = *r.Plan
	}
	source := r.Source
	if r.TokenSource != nil {
		source = *r.TokenSource
	}
	lines = append(lines, fmt.Sprintf("Plan: %s (%s)", st.magenta(plan), source))

	expired := r.TokenExpired != nil && *r.TokenExpired
	if r.TokenExpiresAt != nil {
		expired = *r.TokenExpiresAt < rc.now
	}
	if expired {
		rel := "n/a"
		if r.TokenExpiresAt != nil {
			rel = formatRelative(*r.TokenExpiresAt, rc.now)
		}
		lines = append(lines, st.yellow("OAuth token expired ("+rel+"); "+loginHint))
	}
	if r.Relogin != nil {
		lines = append(lines, st.dim("Relogin: "+*r.Relogin))
	}

	windows := extractWindows(r.payload, rc.loc)
	if len(windows) == 0 {
		lines = append(lines, st.dim("No usage windows in response."))
	}
	for _, w := range windows {
		lines = append(lines, rc.window(w))
	}
	if eu := r.payload.get("extra_usage"); extraUsageActive(eu) {
		lines = append(lines, st.bold("Extra usage")+": "+extraUsageDetail(eu))
	}
	if line := rc.cloudCredit(extractCloudCredit(r.payload, rc.now, rc.loc), false); line != "" {
		lines = append(lines, line)
	}
	for _, w := range r.Warnings {
		lines = append(lines, st.yellow(w))
	}

	fetchedAt := 0.0
	if r.Cache.FetchedAt != nil {
		fetchedAt = *r.Cache.FetchedAt
	}
	fetched := "Fetched: " + formatTimestamp(fetchedAt, rc.loc)
	plain := fetched
	annotated := false
	if r.Source == "config-cache" {
		age := "age unknown"
		if fetchedAt > 0 {
			age = formatRelative(fetchedAt, rc.now)
		}
		fetched += " " + st.yellow("[local cache: "+age+"]")
		plain += " [local cache: " + age + "]"
		annotated = true
	} else if r.Cache.FromCache && r.Cache.StaleReason == nil {
		fetched += " " + st.dim("(cached)")
		plain += " (cached)"
	}

	// Wrapped, never truncated: what identifies the credential sits at
	// the end of the sentence.
	var continuation []string
	if r.Cache.StaleReason != nil {
		lbl := "stale cache"
		if r.Source == "config-cache" {
			lbl = "no live data"
		}
		annotation := "[" + lbl + ": " + *r.Cache.StaleReason + "]"
		if utf8.RuneCountInString(plain)+1+utf8.RuneCountInString(annotation) <= rc.width {
			fetched += " " + st.red(annotation)
		} else {
			continuation = textwrap(annotation, rc.width, "  ")
		}
		annotated = true
	}
	if annotated {
		lines = append(lines, fetched)
	} else {
		lines = append(lines, st.dim(fetched))
	}
	for _, c := range continuation {
		lines = append(lines, st.red(c))
	}
	return strings.Join(lines, "\n")
}

var wsRun = regexp.MustCompile(`\s+`)

// textwrap is Python's textwrap.wrap(text, width, initial_indent=indent,
// subsequent_indent=indent, break_long_words=False,
// break_on_hyphens=False): words are never split, an overlong word gets a
// line of its own, and whitespace at line edges is dropped.
func textwrap(text string, width int, indent string) []string {
	text = strings.ReplaceAll(text, "\t", "        ") // expand_tabs (approximation: no tab stops)
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\v', '\f', '\r':
			return ' '
		}
		return r
	}, text)
	// Chunks alternate words and whitespace runs, as re.split(r'(\s+)').
	var chunks []string
	last := 0
	for _, loc := range wsRun.FindAllStringIndex(text, -1) {
		if loc[0] > last {
			chunks = append(chunks, text[last:loc[0]])
		}
		chunks = append(chunks, text[loc[0]:loc[1]])
		last = loc[1]
	}
	if last < len(text) {
		chunks = append(chunks, text[last:])
	}
	n := utf8.RuneCountInString
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }

	var lines []string
	for len(chunks) > 0 {
		var cur []string
		curLen := 0
		w := width - n(indent)
		if blank(chunks[0]) && len(lines) > 0 {
			chunks = chunks[1:]
		}
		for len(chunks) > 0 {
			l := n(chunks[0])
			if curLen+l > w {
				break
			}
			cur = append(cur, chunks[0])
			curLen += l
			chunks = chunks[1:]
		}
		if len(chunks) > 0 && n(chunks[0]) > w && len(cur) == 0 {
			cur = append(cur, chunks[0])
			chunks = chunks[1:]
		}
		if len(cur) > 0 && blank(cur[len(cur)-1]) {
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			lines = append(lines, indent+strings.Join(cur, ""))
		}
	}
	return lines
}
