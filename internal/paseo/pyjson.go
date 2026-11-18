package paseo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file reproduces the parts of Python's json module the old tool relied
// on, so that registration files round-trip byte for byte and digests match
// hashlib.sha256(json.dumps(value, sort_keys=True).encode()).
//
// Decoded values are: nil, bool, string, json.Number, []any and *Object.
// json.Number keeps the literal, so an integer stays an integer and a float
// stays a float, as Python's json.loads distinguishes them. Values built in
// Go may also use int, int64, float64, []string and map[string]any.

// Object is a JSON object that keeps its key order, as a Python dict does.
type Object struct {
	keys []string
	vals map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{vals: map[string]any{}} }

// Obj builds an object from alternating keys and values.
func Obj(kv ...any) *Object {
	o := NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Get returns the value for key and whether it is present.
func (o *Object) Get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.vals[key]
	return v, ok
}

// Value returns the value for key, nil when absent (Python's dict.get).
func (o *Object) Value(key string) any {
	v, _ := o.Get(key)
	return v
}

// String returns the value for key when it is a string.
func (o *Object) String(key string) (string, bool) {
	s, ok := o.Value(key).(string)
	return s, ok
}

// Object returns the value for key when it is an object.
func (o *Object) Object(key string) (*Object, bool) {
	v, ok := o.Value(key).(*Object)
	return v, ok && v != nil
}

// Set sets key. A new key goes last; an existing key keeps its place, as in
// Python's dict.update.
func (o *Object) Set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Delete removes key.
func (o *Object) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// Keys returns the keys in order.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}

// Len is the number of keys.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Clone returns a shallow copy.
func (o *Object) Clone() *Object {
	c := NewObject()
	for _, k := range o.Keys() {
		c.Set(k, o.vals[k])
	}
	return c
}

// Decode parses one JSON document, keeping object key order and number
// literals. Trailing non-space data is an error, as in Python.
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("extra data after JSON value")
	}
	return v, nil
}

// DecodeObject parses a JSON document that must be an object.
func DecodeObject(data []byte) (*Object, error) {
	v, err := Decode(data)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*Object)
	if !ok {
		return nil, errExpectedObject
	}
	return o, nil
}

var errExpectedObject = errors.New("Expected a JSON object")

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
				key, ok := kt.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(key, v)
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
	default:
		return tok, nil
	}
}

// Dumps is json.dumps(v, sort_keys=sortKeys): separators ", " and ": ",
// ensure_ascii escaping, Python float repr.
func Dumps(v any, sortKeys bool) (string, error) {
	var b strings.Builder
	if err := encode(&b, v, sortKeys, -1, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// DumpIndent is json.dump(v, stream, indent=indent) without sort_keys:
// insertion order, separators "," and ": ".
func DumpIndent(v any, indent int) (string, error) {
	var b strings.Builder
	if err := encode(&b, v, false, indent, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Digest is hashlib.sha256(json.dumps(v, sort_keys=True).encode()).hexdigest().
func Digest(v any) (string, error) {
	s, err := Dumps(v, true)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:]), nil
}

func mustDigest(v any) string {
	d, err := Digest(v)
	if err != nil {
		panic(err)
	}
	return d
}

func newline(b *strings.Builder, indent, level int) {
	b.WriteByte('\n')
	b.WriteString(strings.Repeat(" ", indent*level))
}

func encode(b *strings.Builder, v any, sortKeys bool, indent, level int) error {
	itemSep := ", "
	if indent >= 0 {
		itemSep = ","
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
		writeString(b, t)
	case json.Number:
		s, err := numberLiteral(t)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(FloatRepr(t))
	case []string:
		list := make([]any, len(t))
		for i, s := range t {
			list[i] = s
		}
		return encode(b, list, sortKeys, indent, level)
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteString(itemSep)
			}
			if indent >= 0 {
				newline(b, indent, level+1)
			}
			if err := encode(b, item, sortKeys, indent, level+1); err != nil {
				return err
			}
		}
		if indent >= 0 {
			newline(b, indent, level)
		}
		b.WriteByte(']')
	case map[string]any:
		o := NewObject()
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o.Set(k, t[k])
		}
		return encode(b, o, sortKeys, indent, level)
	case *Object:
		if t.Len() == 0 {
			b.WriteString("{}")
			return nil
		}
		keys := t.Keys()
		if sortKeys {
			// Go compares strings bytewise; for valid UTF-8 that is code
			// point order, which is Python's str order.
			sort.Strings(keys)
		}
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(itemSep)
			}
			if indent >= 0 {
				newline(b, indent, level+1)
			}
			writeString(b, k)
			b.WriteString(": ")
			if err := encode(b, t.vals[k], sortKeys, indent, level+1); err != nil {
				return err
			}
		}
		if indent >= 0 {
			newline(b, indent, level)
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("cannot encode %T", v)
	}
	return nil
}

// isFloatLiteral is Python's rule: a fraction or an exponent makes a float.
func isFloatLiteral(s string) bool { return strings.ContainsAny(s, ".eE") }

// numberLiteral renders a decoded number the way Python would print the
// value json.loads produced from it.
func numberLiteral(n json.Number) (string, error) {
	s := string(n)
	if isFloatLiteral(s) {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return "", err
		}
		return FloatRepr(f), nil
	}
	i, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return "", fmt.Errorf("invalid number %q", s)
	}
	return i.String(), nil
}

// FloatRepr is Python's repr(float), as json.dumps prints floats.
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX, shortest digits
	mant, expPart, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	decpt := exp + 1
	var out string
	if decpt > -4 && decpt <= 16 {
		switch {
		case decpt <= 0:
			out = "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			out = digits[:decpt] + "." + digits[decpt:]
		}
	} else {
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		x := decpt - 1
		es := "+"
		if x < 0 {
			es = "-"
			x = -x
		}
		xs := strconv.Itoa(x)
		if len(xs) < 2 {
			xs = "0" + xs
		}
		out += "e" + es + xs
	}
	return sign + out
}

// writeString writes s with json.dumps' ensure_ascii escaping.
func writeString(b *strings.Builder, s string) {
	const hexDigits = "0123456789abcdef"
	u4 := func(r rune) {
		b.WriteString(`\u`)
		b.WriteByte(hexDigits[(r>>12)&0xf])
		b.WriteByte(hexDigits[(r>>8)&0xf])
		b.WriteByte(hexDigits[(r>>4)&0xf])
		b.WriteByte(hexDigits[r&0xf])
	}
	b.WriteByte('"')
	for _, r := range s {
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
			switch {
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r < 0x10000:
				u4(r)
			default:
				r -= 0x10000
				u4(0xd800 | (r>>10)&0x3ff)
				u4(0xdc00 | r&0x3ff)
			}
		}
	}
	b.WriteByte('"')
}

// pyStr is Python's str() for the scalar values JSON yields. Containers get
// a JSON rendering, which differs from Python's repr; the old tool never
// stringified a container on a path that mattered.
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
	case json.Number:
		s, err := numberLiteral(t)
		if err != nil {
			return string(t)
		}
		if s == "Infinity" {
			return "inf"
		} else if s == "-Infinity" {
			return "-inf"
		}
		return s
	case int:
		return strconv.Itoa(t)
	case float64:
		return FloatRepr(t)
	default:
		s, err := Dumps(v, false)
		if err != nil {
			return fmt.Sprint(v)
		}
		return s
	}
}

// truthy is Python's bool() for decoded values.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return err != nil || f != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case []string:
		return len(t) > 0
	case *Object:
		return t.Len() > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// equal is Python's == for decoded values, by canonical encoding. It differs
// from Python only for 1 == 1.0 and True == 1, which never meet here.
func equal(a, b any) bool {
	x, err1 := Dumps(a, true)
	y, err2 := Dumps(b, true)
	return err1 == nil && err2 == nil && x == y
}

// validUTF8 reports whether raw would decode as Python's UTF-8 does.
func validUTF8(raw []byte) bool { return utf8.Valid(raw) }
