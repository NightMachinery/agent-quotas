package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// value is a decoded JSON value that keeps what Python's json module keeps
// and Go's map decoding loses: object key order, and whether a number was
// written as an integer or as a float. The old tool's output depends on
// both (`parse_utilization` treats 0..1 floats as fractions; `str()` of a
// dict lists keys in insertion order).
type value struct {
	kind byte // 'n' null, 'b' bool, 'i' integer, 'f' float, 's' string, 'a' array, 'o' object
	b    bool
	num  string // the literal, for 'i' and 'f'
	s    string
	arr  []*value
	keys []string
	obj  map[string]*value
}

func parseValue(data []byte) (*value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (*value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case nil:
		return &value{kind: 'n'}, nil
	case bool:
		return &value{kind: 'b', b: t}, nil
	case json.Number:
		lit := string(t)
		if strings.ContainsAny(lit, ".eE") {
			return &value{kind: 'f', num: lit}, nil
		}
		return &value{kind: 'i', num: lit}, nil
	case string:
		return &value{kind: 's', s: t}, nil
	case json.Delim:
		switch t {
		case '[':
			v := &value{kind: 'a'}
			for dec.More() {
				e, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				v.arr = append(v.arr, e)
			}
			_, err := dec.Token()
			return v, err
		case '{':
			v := &value{kind: 'o', obj: map[string]*value{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				e, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := v.obj[k]; !dup {
					v.keys = append(v.keys, k)
				}
				v.obj[k] = e
			}
			_, err := dec.Token()
			return v, err
		}
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

// get returns an object member, or nil.
func (v *value) get(key string) *value {
	if v == nil || v.kind != 'o' {
		return nil
	}
	return v.obj[key]
}

func (v *value) isObject() bool { return v != nil && v.kind == 'o' }
func (v *value) isNull() bool   { return v == nil || v.kind == 'n' }

// isNumber is Python's isinstance(x, (int, float)) and not bool.
func (v *value) isNumber() bool { return v != nil && (v.kind == 'i' || v.kind == 'f') }

func (v *value) float() float64 {
	f, _ := strconv.ParseFloat(v.num, 64)
	return f
}

// str returns a string member's value, or "".
func (v *value) str() (string, bool) {
	if v == nil || v.kind != 's' {
		return "", false
	}
	return v.s, true
}

// truthy is Python truthiness.
func (v *value) truthy() bool {
	if v == nil {
		return false
	}
	switch v.kind {
	case 'n':
		return false
	case 'b':
		return v.b
	case 'i', 'f':
		return v.float() != 0
	case 's':
		return v.s != ""
	case 'a':
		return len(v.arr) > 0
	case 'o':
		return len(v.keys) > 0
	}
	return false
}

// getPath is common_sub_status.get_path: None for a missing key, a
// non-object on the way, or an empty-string/None value.
func (v *value) getPath(path ...string) *value {
	cur := v
	for _, k := range path {
		if !cur.isObject() {
			return nil
		}
		cur = cur.obj[k]
		if cur == nil || cur.kind == 'n' || (cur.kind == 's' && cur.s == "") {
			return nil
		}
	}
	return cur
}

// sortedKeys returns an object's keys in Python's sorted() order (code
// point order, which is UTF-8 byte order).
func (v *value) sortedKeys() []string {
	keys := append([]string(nil), v.keys...)
	sort.Strings(keys)
	return keys
}

// raw re-encodes the value as compact JSON with key order kept.
func (v *value) raw() json.RawMessage {
	var buf bytes.Buffer
	v.encode(&buf)
	return buf.Bytes()
}

func (v *value) encode(buf *bytes.Buffer) {
	if v == nil {
		buf.WriteString("null")
		return
	}
	switch v.kind {
	case 'n':
		buf.WriteString("null")
	case 'b':
		buf.WriteString(strconv.FormatBool(v.b))
	case 'i', 'f':
		buf.WriteString(v.num)
	case 's':
		writeJSONString(buf, v.s)
	case 'a':
		buf.WriteByte('[')
		for i, e := range v.arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			e.encode(buf)
		}
		buf.WriteByte(']')
	case 'o':
		buf.WriteByte('{')
		for i, k := range v.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, k)
			buf.WriteByte(':')
			v.obj[k].encode(buf)
		}
		buf.WriteByte('}')
	}
}

func writeJSONString(buf *bytes.Buffer, s string) {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	buf.Truncate(buf.Len() - 1) // Encode's newline
}

// pyStr is Python's str() of the decoded value: what an f-string prints.
func (v *value) pyStr() string {
	if v != nil && v.kind == 's' {
		return v.s
	}
	return v.pyRepr()
}

// pyRepr is Python's repr() of the decoded value.
func (v *value) pyRepr() string {
	if v == nil {
		return "None"
	}
	switch v.kind {
	case 'n':
		return "None"
	case 'b':
		if v.b {
			return "True"
		}
		return "False"
	case 'i':
		return v.num
	case 'f':
		return pyFloatRepr(v.float())
	case 's':
		return pyStrRepr(v.s)
	case 'a':
		parts := make([]string, len(v.arr))
		for i, e := range v.arr {
			parts[i] = e.pyRepr()
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case 'o':
		parts := make([]string, len(v.keys))
		for i, k := range v.keys {
			parts[i] = pyStrRepr(k) + ": " + v.obj[k].pyRepr()
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

// pyStrRepr is repr() of a str: single quotes unless the text holds a
// single quote and no double quote.
func pyStrRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyFloatRepr is repr() of a Python float: the shortest round-tripping
// digits, fixed notation for exponents in [-4, 16), always with a decimal
// point, scientific (e+XX) otherwise.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	neg := strings.HasPrefix(mant, "-")
	mant = strings.TrimPrefix(mant, "-")
	digits := strings.Replace(mant, ".", "", 1)
	sign := ""
	if neg {
		sign = "-"
	}
	if exp < -4 || exp >= 16 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es = "-"
			exp = -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, es, exp)
	}
	var out string
	if exp < 0 {
		out = "0." + strings.Repeat("0", -exp-1) + digits
	} else if len(digits) <= exp+1 {
		out = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	} else {
		out = digits[:exp+1] + "." + digits[exp+1:]
	}
	return sign + out
}

// pyRound1 is Python's round(x, 1) for a float: correctly rounded, ties to
// even on the exact binary value.
func pyRound1(f float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 1, 64), 64)
	return r
}

// pyG is Python's format(x, "g"): six significant digits, trailing zeros
// dropped.
func pyG(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return pyFloatRepr(f)
	}
	return strconv.FormatFloat(f, 'g', 6, 64)
}

// pyMoney is Python's format(x, ",.2f").
func pyMoney(f float64) string {
	s := strconv.FormatFloat(math.Abs(f), 'f', 2, 64)
	intPart, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	sign := ""
	if math.Signbit(f) { // Python keeps the sign, even of -0.00
		sign = "-"
	}
	return sign + b.String() + "." + frac
}
