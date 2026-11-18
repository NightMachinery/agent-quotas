package paseo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// QuotaError matches the usage-limit wording of every provider (Python's
// QUOTA_ERROR, case-insensitive search).
var QuotaError = regexp.MustCompile(`(?i)usage[_ -]limit|rate[_ -]limit|quota (?:exceeded|exhausted)|` +
	`(?:you(?:'ve| have)?|account) (?:hit|reached|exceeded) .*limit|` +
	`(?:hit|reached) your limit|out of .*usage|too many requests|quota reached`)

// tailBytes is how much of a transcript's end is read.
const tailBytes = 1 << 20

// quotaErrorOf reports whether the record's lastError is a usage limit.
func quotaErrorOf(record *Object) bool {
	s, ok := record.String("lastError")
	return ok && QuotaError.MatchString(s)
}

// quotaFailure ports quota_failure: the identity of a failed turn that Paseo
// recorded with the limit in lastError, or "".
func quotaFailure(record *Object) string {
	if truthy(record.Value("archivedAt")) {
		return ""
	}
	status, _ := record.String("lastStatus")
	if status != "error" && status != "closed" {
		return ""
	}
	if !quotaErrorOf(record) {
		return ""
	}
	// lastActivityAt moves for incidental events; the failed turn is its
	// user message and attention timestamp, plus the error text.
	return mustDigest([]any{record.Value("lastError"), record.Value("lastUserMessageAt"), record.Value("attentionTimestamp")})
}

// parseUUID is str(uuid.UUID(s)): braces, a urn:uuid: prefix and hyphens are
// tolerated; the result is the canonical lowercase form.
func parseUUID(v any) (string, bool) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case json.Number:
		s = string(t)
	default:
		return "", false
	}
	s = strings.ReplaceAll(s, "urn:", "")
	s = strings.ReplaceAll(s, "uuid:", "")
	s = strings.Trim(s, "{}")
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return "", false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return "", false
		}
	}
	s = strings.ToLower(s)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], true
}

// tailLines reads the last n bytes of path and splits them like
// bytes.splitlines (\n, \r\n and \r).
func tailLines(path string, n int64) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	start := end - n
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var lines [][]byte
	for len(data) > 0 {
		i := bytes.IndexAny(data, "\r\n")
		if i < 0 {
			lines = append(lines, data)
			break
		}
		lines = append(lines, data[:i])
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			i++
		}
		data = data[i+1:]
	}
	return lines, nil
}

// decodeLine is json.loads on one transcript line; ok=false where Python
// raises ValueError (bad JSON or bad UTF-8).
func decodeLine(raw []byte) (any, bool) {
	if !validUTF8(raw) {
		return nil, false
	}
	v, err := Decode(raw)
	return v, err == nil
}

// claudeLimitFailure ports claude_limit_failure: a limit stop that Paseo
// records as an ordinary finished turn. Claude Code ends it with a synthetic
// assistant entry flagged isApiErrorMessage with error "rate_limit"; it counts
// while it is still the last user/assistant entry of the transcript.
func claudeLimitFailure(record *Object, nativeHome string, tail int64) (string, error) {
	persistence, _ := record.Value("persistence").(*Object)
	session, ok := parseUUID(persistence.Value("sessionId"))
	if !ok {
		return "", nil
	}
	matches, _ := filepath.Glob(filepath.Join(nativeHome, "projects", "*", session+".jsonl"))
	if len(matches) != 1 {
		return "", nil
	}
	lines, err := tailLines(matches[0], tail)
	if err != nil {
		return "", err
	}
	for i := len(lines) - 1; i >= 0; i-- {
		v, ok := decodeLine(lines[i])
		if !ok {
			continue // the first line of the tail is usually cut in half
		}
		entry, isObj := v.(*Object)
		if !isObj {
			continue
		}
		typ, _ := entry.String("type")
		if typ != "user" && typ != "assistant" {
			continue // queue, mode and summary bookkeeping is not a turn
		}
		if apiErr, _ := entry.Value("isApiErrorMessage").(bool); typ != "assistant" || !apiErr {
			return "", nil
		}
		var content any
		if msg := entry.Value("message"); truthy(msg) {
			m, isObj := msg.(*Object)
			if !isObj {
				return "", errors.New("transcript message is not an object")
			}
			content = m.Value("content")
		}
		var text string
		if list, isList := content.([]any); isList {
			var parts []string
			for _, part := range list {
				p, isObj := part.(*Object)
				if !isObj {
					continue
				}
				t, has := p.Get("text")
				if !has {
					t = ""
				}
				s, isStr := t.(string)
				if !isStr {
					return "", errors.New("transcript text part is not a string")
				}
				parts = append(parts, s)
			}
			text = strings.Join(parts, " ")
		} else if truthy(content) {
			text = pyStr(content)
		}
		if errName, _ := entry.String("error"); errName != "rate_limit" && !QuotaError.MatchString(text) {
			return "", nil
		}
		return mustDigest([]any{"claude-limit", session, entry.Value("uuid"), text}), nil
	}
	return "", nil
}

// codexRollout ports codex_rollout: the session log of a thread. Codex
// thread ids are UUIDv7 (the first 48 bits are the creation time in ms) and
// Codex files the log under sessions/YYYY/MM/DD of that local day; the
// neighbouring days cover a timezone edge, a full search is the last resort.
func codexRollout(nativeHome, threadID string) string {
	sessions := filepath.Join(nativeHome, "sessions")
	name := "rollout-*-" + threadID + ".jsonl"
	hexPart := strings.ReplaceAll(threadID, "-", "")
	if len(hexPart) > 12 {
		hexPart = hexPart[:12]
	}
	if ms, err := strconv.ParseInt(hexPart, 16, 64); err == nil {
		created := time.UnixMilli(ms)
		for _, offset := range []int{0, -1, 1} {
			day := created.Add(time.Duration(offset) * 24 * time.Hour).Local()
			pattern := filepath.Join(sessions, fmt.Sprintf("%04d", day.Year()),
				fmt.Sprintf("%02d", int(day.Month())), fmt.Sprintf("%02d", day.Day()), name)
			if m, _ := filepath.Glob(pattern); len(m) > 0 {
				return m[0]
			}
		}
	}
	if m, _ := filepath.Glob(filepath.Join(sessions, "*", "*", "*", name)); len(m) > 0 {
		return m[0]
	}
	return ""
}

// codexLimitFailure ports codex_limit_failure: Codex ends a limit stop with a
// task_complete whose error names the usage limit; it counts while that is
// the last root turn in the session log.
func codexLimitFailure(record *Object, nativeHome string, tail int64) (string, error) {
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
		return "", nil
	}
	path := codexRollout(nativeHome, id)
	if path == "" {
		return "", nil
	}
	lines, err := tailLines(path, tail)
	if err != nil {
		return "", err
	}
	for i := len(lines) - 1; i >= 0; i-- {
		v, ok := decodeLine(lines[i])
		if !ok {
			continue
		}
		entry, isObj := v.(*Object)
		if !isObj {
			continue
		}
		payload, isObj := entry.Value("payload").(*Object)
		if typ, _ := entry.String("type"); typ != "event_msg" || !isObj {
			continue
		}
		ptype, _ := payload.String("type")
		if ptype != "task_started" && ptype != "task_complete" {
			continue
		}
		if root := payload.Value("root_turn_id"); truthy(root) && !equal(payload.Value("turn_id"), root) {
			continue // a review or compaction sub-turn, not the agent's own turn
		}
		var message string
		var isStr bool
		if ptype == "task_complete" {
			if e, ok := payload.Value("error").(*Object); ok {
				message, isStr = e.String("message")
			}
		}
		if !isStr || !QuotaError.MatchString(message) {
			return "", nil
		}
		return mustDigest([]any{"codex-limit", id, payload.Value("turn_id"), message}), nil
	}
	return "", nil
}

// limitFailure ports limit_failure: a limit stop recorded only in the
// provider's own transcript.
func limitFailure(record *Object, id Identity) (string, error) {
	switch id.Agent {
	case "claude":
		return claudeLimitFailure(record, id.NativeHome, tailBytes)
	case "codex":
		return codexLimitFailure(record, id.NativeHome, tailBytes)
	}
	return "", nil
}
