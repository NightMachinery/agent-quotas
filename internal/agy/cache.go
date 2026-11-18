package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// The statusline cache is one JSON object written by two writers: agy's
// statusline hook (zshlang/wrappers/agy_statusline.dash) and the slow path.
// Both merge per bucket:
//
//	{"account": {"tier": "...", "email": "..."},
//	 "buckets": {"gemini-5h": {"remaining_percent": 98.0, "resets_at": 1789644453,
//	                           "captured_at": 1789130000, "source": "statusline"}}}
//
// Key order is kept on rewrite, as jq kept it, so the file a human reads
// does not reshuffle on every write.

// rawObject is a JSON object with its key order and the raw bytes of each
// value.
type rawObject []rawField

type rawField struct {
	Key   string
	Value json.RawMessage
}

func parseRawObject(data []byte) (rawObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	var o rawObject
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(kt.(string), v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("trailing data after JSON value")
	}
	return o, nil
}

var errNotObject = errors.New("not a JSON object")

func (o rawObject) get(k string) (json.RawMessage, bool) {
	for _, f := range o {
		if f.Key == k {
			return f.Value, true
		}
	}
	return nil, false
}

func (o rawObject) has(k string) bool {
	_, ok := o.get(k)
	return ok
}

// set replaces an existing key in place, or appends a new one.
func (o *rawObject) set(k string, v json.RawMessage) {
	for i := range *o {
		if (*o)[i].Key == k {
			(*o)[i].Value = v
			return
		}
	}
	*o = append(*o, rawField{k, v})
}

func (o rawObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshal(f.Key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.Value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshal is json.Marshal without HTML escaping, as jq writes.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// canonicalKeys maps the statusline's native names of the Argon pool to the
// ids /usage gives the same pool.
var canonicalKeys = map[string]string{
	"gemini-argon-wtu-weekly": "gemini-4-argon-weekly",
	"gemini-argon-5h":         "gemini-4-argon-5h",
}

func canonicalKey(k string) string {
	if c, ok := canonicalKeys[k]; ok {
		return c
	}
	return k
}

// numberOf returns a JSON number's value.
func numberOf(raw json.RawMessage) (float64, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || !(t[0] == '-' || (t[0] >= '0' && t[0] <= '9')) {
		return 0, false // strings, bools and null are not numbers
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

// capture is a bucket's captured_at, or -1 when it has none.
func capture(raw json.RawMessage) float64 {
	o, err := parseRawObject(raw)
	if err != nil {
		return -1
	}
	v, ok := o.get("captured_at")
	if !ok {
		return -1
	}
	if f, ok := numberOf(v); ok {
		return f
	}
	return -1
}

// canonicalBuckets renames alias keys to their canonical ids, keeping the
// entry captured last when an alias and its canonical id are both present
// (on a tie, the later entry wins, as in the jq both writers share).
func canonicalBuckets(buckets rawObject) rawObject {
	var out rawObject
	for _, f := range buckets {
		key := canonicalKey(f.Key)
		if old, ok := out.get(key); !ok || capture(f.Value) >= capture(old) {
			out.set(key, f.Value)
		}
	}
	return out
}

// bucketsOf is `.buckets // {}` of a cache document, as an object.
func bucketsOf(doc rawObject) rawObject {
	raw, ok := doc.get("buckets")
	if !ok {
		return nil
	}
	b, err := parseRawObject(raw)
	if err != nil {
		return nil
	}
	return b
}

// cacheRow is one bucket fresh enough to report.
type cacheRow struct {
	ID        string
	Remaining float64
	ResetsAt  float64
	Captured  float64 // -1 when the bucket has no capture time
	Source    string
	Group     string
	Label     string
}

// cacheRows is h-agy-status-statusline-raw: the buckets with a numeric
// remaining_percent that pass the age filter. maxAge < 0 never drops on age.
func cacheRows(doc rawObject, now float64, maxAge int) []cacheRow {
	var rows []cacheRow
	for _, f := range canonicalBuckets(bucketsOf(doc)) {
		b, err := parseRawObject(f.Value)
		if err != nil {
			continue
		}
		rawPct, ok := b.get("remaining_percent")
		if !ok {
			continue
		}
		pct, ok := numberOf(rawPct)
		if !ok {
			continue
		}
		captured := -1.0
		if v, ok := b.get("captured_at"); ok {
			if c, ok := numberOf(v); ok {
				captured = c
			}
		}
		if maxAge >= 0 && (!hasNumber(b, "captured_at") || now-captured > float64(maxAge)) {
			continue
		}
		row := cacheRow{ID: f.Key, Remaining: pct, Captured: captured, Source: "unknown"}
		if v, ok := b.get("resets_at"); ok {
			if r, ok := numberOf(v); ok {
				row.ResetsAt = r
			}
		}
		row.Source = stringOr(b, "source", "unknown")
		row.Group = stringOr(b, "group", "")
		row.Label = stringOr(b, "label", "")
		rows = append(rows, row)
	}
	return rows
}

func hasNumber(o rawObject, k string) bool {
	v, ok := o.get(k)
	if !ok {
		return false
	}
	_, ok = numberOf(v)
	return ok
}

// stringOr is `.k // dflt` for a string field; a value that is null or
// false falls back, and any other non-string prints as its JSON text.
func stringOr(o rawObject, k, dflt string) string {
	v, ok := o.get(k)
	if !ok {
		return dflt
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return s
	}
	t := string(bytes.TrimSpace(v))
	if t == "null" || t == "false" {
		return dflt
	}
	return t
}

// cacheMetadata is `.account` of the cache when it is an object: the tier
// and email the statusline payload carried.
func cacheMetadata(path string) rawObject {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	doc, err := parseRawObject(data)
	if err != nil {
		return nil
	}
	raw, ok := doc.get("account")
	if !ok {
		return nil
	}
	o, _ := parseRawObject(raw)
	return o
}

// mergeSlow is h-agy-status-cache-merge: the slow path's rows go into the
// cache per bucket (never wholesale), stamped with source "slow" and the
// capture time; every other key and bucket is kept. The write is atomic.
func mergeSlow(path string, rows []usageRow, now int64) error {
	var prev rawObject
	if data, err := os.ReadFile(path); err == nil {
		prev, _ = parseRawObject(data) // a corrupt cache costs its old buckets, not this write
	}
	buckets := canonicalBuckets(bucketsOf(prev))
	for _, r := range rows {
		if r.BucketID == "" {
			continue
		}
		v, err := marshal(struct {
			RemainingPercent float64 `json:"remaining_percent"`
			ResetsAt         int64   `json:"resets_at"`
			Group            string  `json:"group"`
			Label            string  `json:"label"`
			CapturedAt       int64   `json:"captured_at"`
			Source           string  `json:"source"`
		}{r.Remaining, r.ResetsAt, r.Group, r.Label, now, "slow"})
		if err != nil {
			return err
		}
		buckets.set(r.BucketID, v)
	}
	if buckets == nil {
		buckets = rawObject{}
	}
	b, err := buckets.MarshalJSON()
	if err != nil {
		return err
	}
	prev.set("buckets", b)
	doc, err := prev.MarshalJSON()
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, doc, "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	return writeAtomic(path, pretty.Bytes())
}

// writeAtomic writes data to path through a temp file in the same
// directory: the statusline hook and readers may open the file at any
// moment, and must see the old file or the new one, never half of one.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, werr)
	}
	return nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
