package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// LegacyJSON is the agy-status --json document: the account objects (the
// snapshots' Native rows) as one array, indented as jq --slurp printed it.
func LegacyJSON(rows []json.RawMessage) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(r)
	}
	b.WriteByte(']')
	var out bytes.Buffer
	if err := json.Indent(&out, b.Bytes(), "", "  "); err != nil {
		return append(b.Bytes(), '\n')
	}
	out.WriteByte('\n')
	return out.Bytes()
}

// FlatBucket is one bucket of the flat array (`agy_status_json_p=y
// agy-status`): an account object's bucket without used_percent.
type FlatBucket struct {
	Group            string  `json:"group"`
	Label            string  `json:"label"`
	RemainingPercent float64 `json:"remaining_percent"`
	ResetsAt         float64 `json:"resets_at"`
	ResetsAtISO      string  `json:"resets_at_iso"`
	Source           string  `json:"source"`
	BucketID         string  `json:"bucket_id,omitempty"`
}

// FlatJSON is the flat bucket array of one account's object (a snapshot's
// Native row), the contract of the older reset notifiers. An account with an
// error, no sign-in or no buckets is an error, since the array has no room
// to say so.
func FlatJSON(native json.RawMessage) ([]byte, error) {
	var a LegacyAccount
	if err := json.Unmarshal(native, &a); err != nil {
		return nil, err
	}
	switch {
	case a.Error != nil:
		return nil, errors.New(*a.Error)
	case a.Message != nil && len(a.Buckets) == 0:
		return nil, errors.New(*a.Message)
	case len(a.Buckets) == 0:
		return nil, errors.New("no quota buckets")
	}
	flat := make([]FlatBucket, len(a.Buckets))
	for i, b := range a.Buckets {
		flat[i] = FlatBucket{b.Group, b.Label, b.RemainingPercent, b.ResetsAt, b.ResetsAtISO, b.Source, b.BucketID}
	}
	out, err := json.MarshalIndent(flat, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ExitCode is agy-status's exit status for rows: 1 when any account has an
// error (the report is still complete), else 0. Signed out is not an error.
func ExitCode(rows []json.RawMessage) int {
	for _, r := range rows {
		var a struct {
			Error *string `json:"error"`
		}
		if json.Unmarshal(r, &a) != nil || a.Error != nil {
			return 1
		}
	}
	return 0
}

var reWindow = regexp.MustCompile(`-(?:weekly|5h)$`)
var reNotSlug = regexp.MustCompile(`[^a-z0-9]+`)

func normalized(s string) string {
	return strings.Trim(reNotSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// pool is one quota pool that can constrain a model.
type pool struct {
	id        string
	remaining float64
	resetsAt  float64 // Unix seconds; 0 when the report does not say
}

// modelPools is every pool that can constrain the model: its family pool
// (gemini or 3p) and any model pool whose slug the model id starts with. An
// applicable pool without a valid number fails closed.
func modelPools(buckets []json.RawMessage, modelID string) ([]pool, error) {
	slug := normalized(modelID)
	var family string
	switch {
	case strings.HasPrefix(slug, "gemini-"):
		family = "gemini"
	case strings.HasPrefix(slug, "claude-"), strings.HasPrefix(slug, "gpt-"),
		strings.HasPrefix(slug, "openai-"), strings.HasPrefix(slug, "anthropic-"):
		family = "3p"
	default:
		return nil, errors.New("cannot determine quota family for model ID")
	}
	var pools []pool
	for _, raw := range buckets {
		var b map[string]json.RawMessage
		if json.Unmarshal(raw, &b) != nil || b == nil {
			continue
		}
		var id string
		if json.Unmarshal(b["bucket_id"], &id) != nil || !reWindow.MatchString(id) {
			continue
		}
		base := normalized(reWindow.ReplaceAllString(id, ""))
		if base != family && slug != base && !strings.HasPrefix(slug, base+"-") {
			continue
		}
		v, ok := numberOf(b["remaining_percent"])
		if !ok || !finite(v) || v < 0 || v > 100 {
			return nil, errors.New("applicable quota measurement is unknown or invalid")
		}
		p := pool{id: id, remaining: v}
		if t, ok := numberOf(b["resets_at"]); ok && finite(t) && t > 0 {
			p.resetsAt = t
		}
		pools = append(pools, p)
	}
	if len(pools) == 0 {
		return nil, errors.New("no applicable quota measurements")
	}
	return pools, nil
}

// QuotaForModel is quota_for_model of agy_model_catalog.py: the lowest
// remaining percent among every pool that can constrain the model (see
// modelPools), and the bucket ids it used.
func QuotaForModel(buckets []json.RawMessage, modelID string) (float64, []string, error) {
	pools, err := modelPools(buckets, modelID)
	if err != nil {
		return 0, nil, err
	}
	remaining := math.Inf(1)
	var ids []string
	for _, p := range pools {
		remaining = math.Min(remaining, p.remaining)
		ids = append(ids, p.id)
	}
	return remaining, ids, nil
}

// now is the clock of ModelSeat and UsableFor; tests replace it.
var now = time.Now

// ModelSeat is agy-best's rule for one model, with when a blocked model has
// room again: the latest reset among its empty pools (0 when a report does
// not say). An empty pool whose reset has passed does not block: the reading
// predates its new window, as for the snapshot's windows. Only agy's own
// runs refresh the reading, so with every agent of the account stopped this
// is the only way to see the quota come back.
func ModelSeat(s model.Snapshot, modelID string) (usable bool, resetAt float64, err error) {
	buckets, err := accountBuckets(s)
	if err != nil {
		return false, 0, err
	}
	pools, err := modelPools(buckets, modelID)
	if err != nil {
		return false, 0, err
	}
	nowS := float64(now().UnixNano()) / 1e9
	usable = true
	for _, p := range pools {
		if p.remaining <= 0 && (p.resetsAt <= 0 || p.resetsAt > nowS) {
			usable = false
			resetAt = math.Max(resetAt, p.resetsAt)
		}
	}
	return usable, resetAt, nil
}

// UsableFor applies agy-best's rule to one account: the model is usable
// when every applicable pool has quota left or has reset since the reading
// (see ModelSeat). An account with an error, that is signed out, or that has
// no report is an error, never "usable".
func UsableFor(s model.Snapshot, modelID string) (bool, error) {
	usable, _, err := ModelSeat(s, modelID)
	return usable, err
}

// accountBuckets is the bucket list of a signed-in account's report; an
// error, a signed-out account or a missing report is an error.
func accountBuckets(s model.Snapshot) ([]json.RawMessage, error) {
	if len(s.Native) == 0 {
		return nil, errors.New("no agy report for " + s.ID)
	}
	var a struct {
		Error    *string           `json:"error"`
		SignedIn *bool             `json:"signed_in"`
		Buckets  []json.RawMessage `json:"buckets"`
	}
	if err := json.Unmarshal(s.Native, &a); err != nil {
		return nil, err
	}
	if a.Error != nil && *a.Error != "" {
		return nil, errors.New(*a.Error)
	}
	if a.SignedIn != nil && !*a.SignedIn {
		return nil, errors.New(s.ID + " is not signed in")
	}
	return a.Buckets, nil
}
