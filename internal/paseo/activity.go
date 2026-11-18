package paseo

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// KeeperMarker starts every keepalive ping the keeper sends. It is part of
// the API: agent-desk drops prompts that start with it from its instruction
// log, and auto-continue never continues a turn it started.
const KeeperMarker = "[agent-quotas keepalive]"

// CompactText is the keeper's compaction prompt, sent unchanged.
const CompactText = "/compact"

// IsKeeperPrompt reports whether a prompt is one the keeper sends: a ping
// (starts with the marker) or a compaction (the raw /compact, or Claude's
// record of that slash command).
func IsKeeperPrompt(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, KeeperMarker) || t == CompactText ||
		strings.Contains(t, "<command-name>/compact</command-name>")
}

// Activity is what a provider transcript says about an agent's recent turns.
// Times are Unix seconds; 0 means unknown.
type Activity struct {
	// Found is false when no transcript could be read.
	Found bool
	// RealAt is the last model response of a turn not started by a keeper
	// prompt, or that turn's prompt when it has no response yet.
	RealAt float64
	// CacheAt is the last model call of any kind, or a compaction boundary.
	CacheAt float64
	// ContextTokens is the context of the last model call; HasContext is
	// false when unknown (no call, or a compaction since).
	ContextTokens int64
	HasContext    bool
	// LastTurnKeeper is true when the last turn was started by a keeper
	// prompt.
	LastTurnKeeper bool
	// CompactedAt is the last compaction boundary in the transcript.
	CompactedAt float64
}

// parseTime reads an RFC 3339 timestamp as Unix seconds (0 when absent).
func parseTime(v any) float64 {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// ParseTime is parseTime for other packages.
func ParseTime(v any) float64 { return parseTime(v) }

func numberOf(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, err := n.Float64()
	if err != nil {
		return 0, false
	}
	return int64(f), true
}

// promptText returns the text of a Claude user entry when it is a prompt
// (not a tool result, a meta entry or a compaction summary).
func promptText(entry *Object) (string, bool) {
	if truthy(entry.Value("isMeta")) || truthy(entry.Value("isCompactSummary")) {
		return "", false
	}
	msg, _ := entry.Value("message").(*Object)
	if msg == nil {
		return "", false
	}
	switch c := msg.Value("content").(type) {
	case string:
		return c, true
	case []any:
		var parts []string
		for _, part := range c {
			p, ok := part.(*Object)
			if !ok {
				continue
			}
			typ, _ := p.String("type")
			if typ == "tool_result" {
				return "", false
			}
			if typ == "text" {
				s, _ := p.String("text")
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, " "), true
	}
	return "", false
}

// scanClaude reads a Claude Code transcript tail.
func scanClaude(lines [][]byte) Activity {
	a := Activity{Found: true}
	keeperTurn := false
	for _, raw := range lines {
		v, ok := decodeLine(raw)
		if !ok {
			continue
		}
		entry, ok := v.(*Object)
		if !ok || truthy(entry.Value("isSidechain")) {
			continue
		}
		at := parseTime(entry.Value("timestamp"))
		typ, _ := entry.String("type")
		switch typ {
		case "user":
			text, isPrompt := promptText(entry)
			if !isPrompt {
				continue
			}
			keeperTurn = IsKeeperPrompt(text)
			if !keeperTurn && at > a.RealAt {
				a.RealAt = at
			}
		case "assistant":
			if apiErr, _ := entry.Value("isApiErrorMessage").(bool); apiErr {
				continue // a synthetic stop, not a model call
			}
			msg, _ := entry.Value("message").(*Object)
			if msg == nil {
				continue
			}
			if m, _ := msg.String("model"); m == "<synthetic>" {
				continue
			}
			if at > a.CacheAt {
				a.CacheAt = at
			}
			if !keeperTurn && at > a.RealAt {
				a.RealAt = at
			}
			if usage, ok := msg.Value("usage").(*Object); ok {
				var total int64
				seen := false
				for _, k := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
					if n, ok := numberOf(usage.Value(k)); ok {
						total += n
						seen = true
					}
				}
				if seen {
					a.ContextTokens, a.HasContext = total, true
				}
			}
		case "system":
			if sub, _ := entry.String("subtype"); sub == "compact_boundary" {
				a.CompactedAt = at
				if at > a.CacheAt {
					a.CacheAt = at
				}
				a.ContextTokens, a.HasContext = 0, false
			}
		}
	}
	a.LastTurnKeeper = keeperTurn
	return a
}

// scanCodex reads a Codex rollout tail. Turns are delimited by root
// task_started events; a turn is a keeper turn when its prompt is a keeper
// prompt, or when it has no prompt and compacts (a manual /compact).
func scanCodex(lines [][]byte) Activity {
	a := Activity{Found: true}
	type turn struct {
		keeper, prompted bool
		start, last      float64
	}
	cur := turn{}
	settle := func(t turn) {
		if t.keeper {
			return
		}
		if t.last > a.RealAt {
			a.RealAt = t.last
		}
	}
	for _, raw := range lines {
		v, ok := decodeLine(raw)
		if !ok {
			continue
		}
		entry, ok := v.(*Object)
		if !ok {
			continue
		}
		at := parseTime(entry.Value("timestamp"))
		typ, _ := entry.String("type")
		payload, _ := entry.Value("payload").(*Object)
		if typ == "compacted" {
			a.CompactedAt = at
			if at > a.CacheAt {
				a.CacheAt = at
			}
			a.ContextTokens, a.HasContext = 0, false
			if !cur.prompted {
				cur.keeper = true
			}
			continue
		}
		if typ != "event_msg" || payload == nil {
			continue
		}
		ptype, _ := payload.String("type")
		switch ptype {
		case "task_started":
			if root := payload.Value("root_turn_id"); truthy(root) && !equal(payload.Value("turn_id"), root) {
				continue
			}
			settle(cur)
			cur = turn{start: at, last: at}
		case "user_message":
			text, _ := payload.String("message")
			cur.prompted = true
			cur.keeper = IsKeeperPrompt(text)
			if at > cur.last {
				cur.last = at
			}
		case "context_compacted":
			a.CompactedAt = at
			a.ContextTokens, a.HasContext = 0, false
			if !cur.prompted {
				cur.keeper = true
			}
		case "token_count":
			info, ok := payload.Value("info").(*Object)
			if !ok {
				continue
			}
			if at > a.CacheAt {
				a.CacheAt = at
			}
			if at > cur.last {
				cur.last = at
			}
			if last, ok := info.Value("last_token_usage").(*Object); ok {
				if n, ok := numberOf(last.Value("input_tokens")); ok {
					a.ContextTokens, a.HasContext = n, true
				}
			}
		}
	}
	settle(cur)
	a.LastTurnKeeper = cur.keeper
	return a
}

// claudeTranscript is the transcript of a Claude agent's session, or "".
func claudeTranscript(record *Object, nativeHome string) string {
	persistence, _ := record.Value("persistence").(*Object)
	session, ok := parseUUID(persistence.Value("sessionId"))
	if !ok {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(nativeHome, "projects", "*", session+".jsonl"))
	if len(matches) != 1 {
		return ""
	}
	return matches[0]
}

// codexTranscript is the rollout of a Codex agent's thread, or "".
func codexTranscript(record *Object, nativeHome string) string {
	persistence, _ := record.Value("persistence").(*Object)
	var thread any
	if md, ok := persistence.Value("metadata").(*Object); ok {
		thread = md.Value("threadId")
	}
	if !truthy(thread) {
		thread = persistence.Value("sessionId")
	}
	id, ok := parseUUID(thread)
	if !ok {
		return ""
	}
	return codexRollout(nativeHome, id)
}

// ActivityOf reads the activity of an agent from its provider transcript
// (the last MiB of it). A missing transcript gives Found false and no error.
func ActivityOf(record *Object, id Identity) (Activity, error) {
	var path string
	var scan func([][]byte) Activity
	switch id.Agent {
	case "claude":
		path, scan = claudeTranscript(record, id.NativeHome), scanClaude
	case "codex":
		path, scan = codexTranscript(record, id.NativeHome), scanCodex
	}
	if path == "" {
		return Activity{}, nil
	}
	lines, err := tailLines(path, tailBytes)
	if err != nil {
		return Activity{}, err
	}
	return scan(lines), nil
}

// CanonicalID is the canonical lowercase form of an agent UUID.
func CanonicalID(s string) (string, bool) { return parseUUID(s) }
