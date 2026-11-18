package codex

// This file holds a small JSON value model that behaves like Python's json
// module, so the legacy documents of codex_status.py can be reproduced byte
// for byte: objects keep their key order, integers stay integers, floats are
// printed the way Python's repr prints them, and strings are escaped the way
// json.dumps(..., ensure_ascii=False) escapes them.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// pyInt is a JSON integer, kept as its literal so large values survive.
type pyInt string

// obj is a JSON object that remembers its key order.
type obj struct {
	keys []string
	m    map[string]any
}

func newObj() *obj { return &obj{m: map[string]any{}} }

// Set adds or replaces a key. A replaced key keeps its position, as in a
// Python dict.
func (o *obj) Set(k string, v any) *obj {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
	return o
}

// Get returns the value of k and whether it is present.
func (o *obj) Get(k string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.m[k]
	return v, ok
}

// V returns the value of k, or nil.
func (o *obj) V(k string) any {
	v, _ := o.Get(k)
	return v
}

// Has reports whether k is present.
func (o *obj) Has(k string) bool {
	_, ok := o.Get(k)
	return ok
}

// Len is the number of keys.
func (o *obj) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Keys returns the keys in order.
func (o *obj) Keys() []string {
	if o == nil {
		return nil
	}
	return o.keys
}

// asObj is Python's `value if isinstance(value, dict) else {}`, except that
// it returns nil for "not a dict"; every obj method accepts a nil receiver.
func asObj(v any) *obj {
	o, _ := v.(*obj)
	return o
}

// isObj reports whether v is a JSON object.
func isObj(v any) bool {
	_, ok := v.(*obj)
	return ok
}

// parseJSON decodes one JSON document into the value model.
func parseJSON(data []byte) (any, error) {
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

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObj()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case json.Number:
		s := string(t)
		if strings.ContainsAny(s, ".eE") {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return nil, err
			}
			return f, nil
		}
		return pyInt(canonicalInt(s)), nil
	default:
		return t, nil // nil, bool, string
	}
}

func canonicalInt(s string) string {
	if s == "-0" {
		return "0"
	}
	return s
}

// numeric is codex_rate_limits.numeric: ints and floats, never bools.
func numeric(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case pyInt:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// jsonNumber is codex_rate_limits.json_number: integral floats become ints.
func jsonNumber(f *float64) any {
	if f == nil {
		return nil
	}
	if !math.IsInf(*f, 0) && !math.IsNaN(*f) && *f == math.Trunc(*f) {
		return pyInt(strconv.FormatFloat(*f, 'f', 0, 64))
	}
	return *f
}

// truthy is Python truthiness for JSON values.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case pyInt:
		f, _ := numeric(t)
		return f != 0
	case []any:
		return len(t) > 0
	case *obj:
		return t.Len() > 0
	}
	return true
}

// emptyish is Python's `value in (None, "")`.
func emptyish(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// pyStr is Python's str() for JSON values.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return t
	case float64:
		return pyFloatRepr(t)
	case pyInt:
		return string(t)
	default:
		var b bytes.Buffer
		writeJSON(&b, v, -1, 0)
		return b.String()
	}
}

// pyFloatRepr is Python's repr(float).
func pyFloatRepr(f float64) string {
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
			return "-0.0"
		}
		return "0.0"
	}
	// Shortest round-tripping digits and the decimal exponent.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	neg := strings.HasPrefix(e, "-")
	e = strings.TrimPrefix(e, "-")
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	var out string
	if exp < -4 || exp >= 16 {
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		out += fmt.Sprintf("e%s%02d", sign, exp)
	} else if exp < 0 {
		out = "0." + strings.Repeat("0", -exp-1) + digits
	} else if len(digits) <= exp+1 {
		out = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	} else {
		out = digits[:exp+1] + "." + digits[exp+1:]
	}
	if neg {
		out = "-" + out
	}
	return out
}

// pyG is Python's format(f, "g"): six significant digits, trailing zeros
// dropped, scientific notation outside 1e-4 <= |f| < 1e6.
func pyG(f float64) string {
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
	const p = 6
	e := strconv.FormatFloat(f, 'e', p-1, 64)
	_, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	if exp < -4 || exp >= p {
		mant, _, _ := strings.Cut(e, "e")
		if strings.Contains(mant, ".") {
			mant = strings.TrimRight(strings.TrimRight(mant, "0"), ".")
		}
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		return fmt.Sprintf("%se%s%02d", mant, sign, exp)
	}
	s := strconv.FormatFloat(f, 'f', p-1-exp, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// pyRound1 is Python's round(f, 1).
func pyRound1(f float64) float64 {
	r, err := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 1, 64), 64)
	if err != nil {
		return f
	}
	return r
}

// pySum is Python's sum() of floats, which since 3.12 uses Neumaier's
// compensated summation.
func pySum(values []float64) float64 {
	var sum, c float64
	for _, x := range values {
		t := sum + x
		if math.Abs(sum) >= math.Abs(x) {
			c += (sum - t) + x
		} else {
			c += (x - t) + sum
		}
		sum = t
	}
	if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
		sum += c
	}
	return sum
}

// dumpIndent is json.dumps(v, indent=2, ensure_ascii=False).
func dumpIndent(v any) []byte {
	var b bytes.Buffer
	writeJSON(&b, v, 2, 0)
	return b.Bytes()
}

// dumpCompact is json.dumps(v, ensure_ascii=False) with the default
// separators (", " and ": ").
func dumpCompact(v any) string {
	var b bytes.Buffer
	writeJSON(&b, v, -1, 0)
	return b.String()
}

// dumpTight is json.dumps(v, separators=(",", ":"), ensure_ascii=False),
// used for the wire and for Snapshot.Native.
func dumpTight(v any) []byte {
	var b bytes.Buffer
	writeJSON(&b, v, -2, 0)
	return b.Bytes()
}

// writeJSON writes v. indent >= 0 is pretty printing; -1 is Python's default
// one-line form; -2 is the tight form.
func writeJSON(b *bytes.Buffer, v any, indent, depth int) {
	itemSep, keySep := ", ", ": "
	if indent >= 0 {
		itemSep = ","
	}
	if indent == -2 {
		itemSep, keySep = ",", ":"
	}
	newline := func(d int) {
		if indent >= 0 {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", indent*d))
		}
	}
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writePyString(b, t)
	case pyInt:
		b.WriteString(string(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case float64:
		switch {
		case math.IsNaN(t):
			b.WriteString("NaN")
		case math.IsInf(t, 1):
			b.WriteString("Infinity")
		case math.IsInf(t, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(pyFloatRepr(t))
		}
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(itemSep)
			}
			newline(depth + 1)
			writeJSON(b, x, indent, depth+1)
		}
		newline(depth)
		b.WriteByte(']')
	case *obj:
		if t.Len() == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteString(itemSep)
			}
			newline(depth + 1)
			writePyString(b, k)
			b.WriteString(keySep)
			writeJSON(b, t.m[k], indent, depth+1)
		}
		newline(depth)
		b.WriteByte('}')
	default:
		// Not part of the model; fall back to encoding/json.
		data, _ := json.Marshal(t)
		b.Write(data)
	}
}

// writePyString escapes like json.dumps with ensure_ascii=False.
func writePyString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}
