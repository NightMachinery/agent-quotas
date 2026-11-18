package render

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Clean is the Python renderers' clean(): str(value or "") without control
// characters (every Unicode category starting with C).
func Clean(v any) string {
	if !truthy(v) {
		return ""
	}
	return cleanString(pyStr(v))
}

func cleanString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !isControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Width is the display width of s in terminal cells: combining marks take
// none, East Asian wide and full-width characters take two.
func Width(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

func runeWidth(r rune) int {
	switch {
	case isMark(r):
		return 0
	case isWide(r):
		return 2
	}
	return 1
}

// Clip cleans s and cuts it to columns cells, ending in "…" when cut.
func Clip(s string, columns int) string {
	s = cleanString(s)
	if Width(s) <= columns {
		return s
	}
	limit := columns - 1
	if limit < 0 {
		limit = 0
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		if w+runeWidth(r) > limit {
			break
		}
		w += runeWidth(r)
		b.WriteRune(r)
	}
	if columns > 0 {
		b.WriteString("…")
	}
	return b.String()
}

// Pad clips s and pads it with spaces to columns cells.
func Pad(s string, columns int, right bool) string {
	s = Clip(s, columns)
	n := columns - Width(s)
	if n < 0 {
		n = 0
	}
	if right {
		return strings.Repeat(" ", n) + s
	}
	return s + strings.Repeat(" ", n)
}

// Duration formats seconds with at most two units: "4d 7h", "1h 25m",
// "12m", "<1m".
func Duration(seconds float64) string {
	s := int64(math.Trunc(seconds))
	if s < 0 {
		s = 0
	}
	var cells []string
	for _, u := range []struct {
		size int64
		unit string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}} {
		amount := s / u.size
		s %= u.size
		if amount != 0 {
			cells = append(cells, strconv.FormatInt(amount, 10)+u.unit)
		}
		if len(cells) == 2 {
			break
		}
	}
	if len(cells) == 0 {
		return "<1m"
	}
	return strings.Join(cells, " ")
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// resetText is reset_text(): the relative reset and the local absolute stamp.
func resetText(epoch any, now float64, loc *time.Location) (string, string) {
	n, ok := asNumLoose(epoch)
	if !ok || !finite(n.F) || n.F <= 0 {
		return "reset unknown", ""
	}
	delta := n.F - now
	relative := "reset " + Duration(-delta) + " ago"
	if delta > 0 {
		relative = "resets in " + Duration(delta)
	}
	if n.F > 253402300799 { // past year 9999: Python's datetime refuses it
		return relative, ""
	}
	t := epochTime(n.F).In(loc)
	return relative, t.Format("Mon 02 Jan 15:04 MST")
}

func epochTime(f float64) time.Time {
	sec := math.Floor(f)
	return time.Unix(int64(sec), int64(math.Round((f-sec)*1e6))*1000)
}

// pyTitle is Python's str.title().
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		cased := unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
		switch {
		case cased && !prevCased:
			b.WriteRune(unicode.ToTitle(r))
		case cased:
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(r)
		}
		prevCased = cased
	}
	return b.String()
}

// pyIsSpace is Python's str.isspace() for one character.
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func rstrip(s string) string { return strings.TrimRightFunc(s, pyIsSpace) }

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// casefold approximates str.casefold() (full case folding) with lower case.
func casefold(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "ß", "ss")
}

// fmtMoney is Python's f"{x:,.2f}".
func fmtMoney(f float64) string {
	s := strconv.FormatFloat(math.Abs(f), 'f', 2, 64)
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + "." + frac
	if f < 0 || (f == 0 && math.Signbit(f)) {
		out = "-" + out
	}
	return out
}

// fmtG is Python's f"{x:g}".
func fmtG(n Num) string {
	f := n.F
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0"
		}
		return "0"
	}
	exp := int(math.Floor(math.Log10(math.Abs(f))))
	// Recompute the exponent after rounding to six significant digits.
	e := strconv.FormatFloat(f, 'e', 5, 64)
	_, es, _ := strings.Cut(e, "e")
	exp, _ = strconv.Atoi(es)
	if exp < -4 || exp >= 6 {
		mant, _, _ := strings.Cut(e, "e")
		if strings.Contains(mant, ".") {
			mant = strings.TrimRight(strings.TrimRight(mant, "0"), ".")
		}
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		es := strconv.Itoa(exp)
		if len(es) < 2 {
			es = "0" + es
		}
		return mant + "e" + sign + es
	}
	s := strconv.FormatFloat(f, 'f', 5-exp, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// fmtDayMonth is time.strftime('%a %-d %b %H:%M').
func fmtDayMonth(t time.Time) string {
	return t.Format("Mon 2 Jan 15:04")
}
