package claude

import (
	"bytes"
	"encoding/json"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// rowKind peeks at a legacy row: an error row has a non-null "error".
type rowHead struct {
	Profile *string `json:"profile"`
	Error   *string `json:"error"`
}

func decodeHead(row json.RawMessage) rowHead {
	var h rowHead
	_ = json.Unmarshal(row, &h)
	return h
}

// LegacyRow returns the snapshot's claude_code_usage.py row, adjusted for
// how the daemon got it. With fromDaemonCache, a reading the daemon fetched
// earlier is marked the way the old TTL cache marked one (source
// "api-cache", from_cache true, fetched_at the upstream time), so the human
// report shows "(cached)" and its timestamp. A snapshot the daemon marked
// stale (back-off, failed refresh) carries its reason as the cache's
// stale_reason, which renders as "[stale cache: ...]".
func LegacyRow(s model.Snapshot, fromDaemonCache bool) json.RawMessage {
	if len(s.Native) == 0 || string(s.Native) == "null" {
		msg := "no usage data available"
		if s.Error != nil {
			msg = *s.Error
		}
		return mustJSON(errorRow{Profile: strPtrOrNil(s.Name), Error: msg})
	}
	if decodeHead(s.Native).Error != nil {
		return s.Native
	}
	var row legacyRow
	if err := json.Unmarshal(s.Native, &row); err != nil {
		return s.Native
	}
	if row.Source != "api" {
		return s.Native // config-cache rows already say what they are
	}
	changed := false
	if fromDaemonCache {
		row.Source = "api-cache"
		row.Cache.FromCache = true
		if s.FetchedAt != nil {
			row.Cache.FetchedAt = model.F(*s.FetchedAt)
		}
		changed = true
	}
	if s.Stale && s.StaleReason != nil {
		row.Source = "api-cache"
		row.Cache.FromCache = true
		row.Cache.StaleReason = model.S(*s.StaleReason)
		changed = true
	}
	if !changed {
		return s.Native
	}
	return mustJSON(row)
}

// LegacyJSON is the --json output: a bare object for one profile, an array
// with all (the old --all). It ends with a newline, as print() did.
func LegacyJSON(rows []json.RawMessage, all bool) []byte {
	var v any = rows
	if !all {
		if len(rows) == 0 {
			v = nil
		} else {
			v = rows[0]
		}
	} else if rows == nil {
		v = []json.RawMessage{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return []byte("null\n")
	}
	return buf.Bytes()
}

// LegacyExitCode is the old exit status: 1 when any profile had nothing to
// show, else 0.
func LegacyExitCode(rows []json.RawMessage) int {
	for _, r := range rows {
		if decodeHead(r).Error != nil {
			return 1
		}
	}
	return 0
}
