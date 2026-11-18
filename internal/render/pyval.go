package render

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
	"unicode/utf8"
)

// The old renderers were Python and worked on decoded JSON as dicts, ints and
// floats. Their output depends on details of that model: dict key order,
// whether a number was an int or a float (52 against 52.0), and Python's
// str() of a value inside a note. This file is the small part of that model
// the renderers need, so the Go port prints the same bytes.

// Object is a JSON object that keeps its key order, like a Python dict.
type Object struct {
	keys []string
	vals map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{vals: map[string]any{}} }

// Get returns the value of k, or nil. A nil object has no keys.
func (o *Object) Get(k string) any {
	if o == nil {
		return nil
	}
	return o.vals[k]
}

// Has reports whether k is present (even with a null value).
func (o *Object) Has(k string) bool {
	if o == nil {
		return false
	}
	_, ok := o.vals[k]
	return ok
}

// Set sets k, keeping the position of an existing key.
func (o *Object) Set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// Keys returns the keys in order.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return o.keys
}

// Len is the number of keys.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Copy returns a shallow copy.
func (o *Object) Copy() *Object {
	c := NewObject()
	for _, k := range o.Keys() {
		c.Set(k, o.vals[k])
	}
	return c
}

// Num is a JSON number with Python's int/float distinction.
type Num struct {
	F     float64
	IsInt bool
	lit   string // the literal of an int, so big ints print as given
}

// Int returns an int Num.
func Int(v int64) Num { return Num{F: float64(v), IsInt: true} }

// Float returns a float Num.
func Float(v float64) Num { return Num{F: v} }

func numFromLiteral(s string) (Num, error) {
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return Num{}, err
		}
		return Num{F: f}, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return Num{}, err
	}
	lit := s
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		lit = strconv.FormatInt(i, 10) // "-0" is 0 in Python
	}
	return Num{F: f, IsInt: true, lit: lit}, nil
}

// String is Python's str() of the number.
func (n Num) String() string {
	if n.IsInt {
		if n.lit != "" {
			return n.lit
		}
		return strconv.FormatInt(int64(n.F), 10)
	}
	return pyFloatRepr(n.F)
}

// pyFloatRepr is Python's repr() of a float.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	if exp < -4 || exp >= 16 {
		_ = mant
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// Decode parses JSON into Objects, []any, Num, string, bool and nil.
func Decode(data []byte) (any, error) {
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
			o := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
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
			list := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case json.Number:
		return numFromLiteral(string(t))
	default:
		return t, nil // string, bool, nil
	}
}

func asObject(v any) *Object {
	o, _ := v.(*Object)
	return o
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// asNum reports a number, excluding bools (Python code that checks
// isinstance(x, bool) first).
func asNum(v any) (Num, bool) {
	n, ok := v.(Num)
	return n, ok
}

// asNumLoose is isinstance(x, (int, float)), which in Python admits bools.
func asNumLoose(v any) (Num, bool) {
	switch t := v.(type) {
	case Num:
		return t, true
	case bool:
		if t {
			return Int(1), true
		}
		return Int(0), true
	}
	return Num{}, false
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case Num:
		return t.F != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case *Object:
		return t.Len() > 0
	}
	return true
}

// or is Python's `a or b`.
func or(a, b any) any {
	if truthy(a) {
		return a
	}
	return b
}

// pyStr is Python's str() of a decoded JSON value.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case Num:
		return t.String()
	case string:
		return t
	}
	return pyRepr(v)
}

// pyRepr is Python's repr() of a decoded JSON value.
func pyRepr(v any) string {
	switch t := v.(type) {
	case string:
		return pyStrRepr(t)
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = pyRepr(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Object:
		parts := make([]string, 0, t.Len())
		for _, k := range t.Keys() {
			parts = append(parts, pyStrRepr(k)+": "+pyRepr(t.Get(k)))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStr(v)
}

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
		case isControl(r) && r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case isControl(r) && r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		case isControl(r):
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyFloat is Python's float() of a JSON value; ok is false where Python
// would raise.
func pyFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case Num:
		return t.F, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// pyJSON writes v the way Python's json.dump(v, ensure_ascii=False) does,
// with the default ", " and ": " separators.
func pyJSON(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case Num:
		switch {
		case t.IsInt:
			b.WriteString(t.String())
		case math.IsNaN(t.F):
			b.WriteString("NaN")
		case math.IsInf(t.F, 1):
			b.WriteString("Infinity")
		case math.IsInf(t.F, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(pyFloatRepr(t.F))
		}
	case string:
		pyJSONString(b, t)
	case []any:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			pyJSON(b, x)
		}
		b.WriteByte(']')
	case *Object:
		b.WriteByte('{')
		for i, k := range t.Keys() {
			if i > 0 {
				b.WriteString(", ")
			}
			pyJSONString(b, k)
			b.WriteString(": ")
			pyJSON(b, t.Get(k))
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("pyJSON: unsupported %T", v))
	}
}

func pyJSONString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

type runeRange struct{ lo, hi rune }

func inRanges(table []runeRange, r rune) bool {
	i := sort.Search(len(table), func(i int) bool { return table[i].hi >= r })
	return i < len(table) && table[i].lo <= r
}

func isControl(r rune) bool { return inRanges(controlRanges, r) }
func isMark(r rune) bool    { return inRanges(markRanges, r) }
func isWide(r rune) bool    { return inRanges(wideRanges, r) }
