package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"

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

// QuotaForModel is quota_for_model of agy_model_catalog.py: the lowest
// remaining percent among every pool that can constrain the model (its
// family pool, gemini or 3p, and any model pool whose slug the model id
// starts with), and the bucket ids it used. An applicable pool without a
// valid number fails closed.
func QuotaForModel(buckets []json.RawMessage, modelID string) (float64, []string, error) {
	slug := normalized(modelID)
	var family string
	switch {
	case strings.HasPrefix(slug, "gemini-"):
		family = "gemini"
	case strings.HasPrefix(slug, "claude-"), strings.HasPrefix(slug, "gpt-"),
		strings.HasPrefix(slug, "openai-"), strings.HasPrefix(slug, "anthropic-"):
		family = "3p"
	default:
		return 0, nil, errors.New("cannot determine quota family for model ID")
	}
	remaining := math.Inf(1)
	var ids []string
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
			return 0, nil, errors.New("applicable quota measurement is unknown or invalid")
		}
		remaining = math.Min(remaining, v)
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, nil, errors.New("no applicable quota measurements")
	}
	return remaining, ids, nil
}

// UsableFor applies agy-best's rule to one account: the model is usable
// when every applicable pool has quota left. An account with an error, that
// is signed out, or that has no report is an error, never "usable".
func UsableFor(s model.Snapshot, modelID string) (bool, error) {
	if len(s.Native) == 0 {
		return false, errors.New("no agy report for " + s.ID)
	}
	var a struct {
		Error    *string           `json:"error"`
		SignedIn *bool             `json:"signed_in"`
		Buckets  []json.RawMessage `json:"buckets"`
	}
	if err := json.Unmarshal(s.Native, &a); err != nil {
		return false, err
	}
	if a.Error != nil && *a.Error != "" {
		return false, errors.New(*a.Error)
	}
	if a.SignedIn != nil && !*a.SignedIn {
		return false, errors.New(s.ID + " is not signed in")
	}
	remaining, _, err := QuotaForModel(a.Buckets, modelID)
	if err != nil {
		return false, err
	}
	return remaining > 0, nil
}
