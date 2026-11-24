package paseo

import (
	"context"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Antigravity (agy) agents. agy-acp ends a turn that hits the account's
// quota with an ordinary reply ("⚠ Individual quota reached. ... Resets in
// 2h19m26s."), so Paseo records the agent idle with no error, and agy's own
// transcript does not hold the notice either. The stop is read from the end
// of Paseo's timeline instead (paseo logs --tail), cached per agent until
// its record changes. Activity and compactions come from agy's transcript.

// AgentAgy is the Identity.Agent of Antigravity providers.
const AgentAgy = "agy"

// agyTimelineTail is how many timeline items a stop check reads.
const agyTimelineTail = 30

// agyTailTimeout bounds one timeline read.
const agyTailTimeout = 30 * time.Second

// Tailer reads the last n items of an agent's Paseo timeline. CLI
// implements it; an engine whose Paseo does not cannot see agy stops.
type Tailer interface {
	Tail(ctx context.Context, agentID, home string, n int) ([]*Object, error)
}

// AgyStop is a quota stop read from an Antigravity agent's timeline.
type AgyStop struct {
	// Failure identifies the stopped turn; "" when the last turn did not
	// stop on quota.
	Failure string `json:"failure"`
	// ResetAt is when the notice says the quota resets (Unix seconds,
	// counted from the record's lastActivityAt); 0 when it does not say.
	ResetAt float64 `json:"reset_at,omitempty"`
}

// agyIdentity is the quota identity of an Antigravity provider: the agy
// account as the profile, and agy's app data directory (where its
// conversations live) as the native home.
func (e *Engine) agyIdentity(home string, provider any, account string) (Identity, error) {
	provs, err := providers(home)
	if err != nil {
		return Identity{}, err
	}
	result, env := NewObject(), NewObject()
	seen := map[string]bool{}
	current := provider
	for {
		name, ok := current.(string)
		entry, isObj := provs.Object(name)
		if !ok || seen[name] || !isObj {
			break
		}
		seen[name] = true
		entryEnv, err := envOf(entry)
		if err != nil {
			return Identity{}, err
		}
		mergeUnder(result, entry, "label", "extends", "env")
		mergeUnder(env, entryEnv)
		current = entry.Value("extends")
	}
	result.Set("env", env)
	result.Set("agy_account", account)
	argv, _ := result.Value("command").([]any)
	hash, err := Digest(result)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Agent: AgentAgy, Profile: account, NativeHome: agyAppDir(argv, env, e.userHome()), ConfigHash: hash}, nil
}

// agyAppDir is where agy keeps its conversations (brain/<id>/...): the
// --app_data_dir flag, taken relative to the gemini directory when
// relative; else <gemini directory>/antigravity-cli. The gemini directory
// is --gemini_dir, else ANTIGRAVITY_GEMINI_DIR, else ~/.gemini.
func agyAppDir(argv []any, env *Object, userHome string) string {
	flag := func(name string) string {
		for i, a := range argv {
			s, _ := a.(string)
			if v, ok := strings.CutPrefix(s, "--"+name+"="); ok {
				return v
			}
			if s == "--"+name && i+1 < len(argv) {
				v, _ := argv[i+1].(string)
				return v
			}
		}
		return ""
	}
	gemini := flag("gemini_dir")
	if gemini == "" {
		gemini, _ = env.Value("ANTIGRAVITY_GEMINI_DIR").(string)
	}
	if gemini == "" {
		gemini = "~/.gemini"
	}
	gemini = expandUser(gemini, userHome)
	app := flag("app_data_dir")
	if app == "" {
		app = filepath.Join(gemini, "antigravity-cli")
	} else if app = expandUser(app, userHome); !filepath.IsAbs(app) {
		app = filepath.Join(gemini, app)
	}
	return realpath(app)
}

// agyResets reads the "Resets in 2h19m26s" of a quota notice.
var agyResets = regexp.MustCompile(`(?i)resets in\s+(?:(\d+)\s*d)?\s*(?:(\d+)\s*h)?\s*(?:(\d+)\s*m)?\s*(?:(\d+)\s*s)?`)

// agyResetIn is the wait a quota notice names, or 0.
func agyResetIn(text string) time.Duration {
	m := agyResets.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	var d time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
		if n, err := strconv.Atoi(m[i+1]); err == nil {
			d += time.Duration(n) * unit
		}
	}
	return d
}

// agyNotice reports whether a reply is agy's quota notice: its first line
// opens with a warning sign and names the limit ("⚠ Individual quota
// reached. Please upgrade ... Resets in 2h19m26s."). Only the opening
// counts, so a report that discusses quotas does not.
func agyNotice(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "⚠") {
		return false
	}
	first, _, _ := strings.Cut(t, "\n")
	return QuotaError.MatchString(first)
}

// agyStopIn finds a quota stop at the end of timeline items: the last model
// output (a reply, reasoning, or a tool call of the agent's own) is agy's
// quota notice. Messages to the agent after it are skipped: a turn
// one of them started either hit the limit again, ending in another notice,
// or is still running, and then the record is not idle.
func agyStopIn(items []*Object) (messageID, text string, ok bool) {
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		switch typ, _ := it.String("type"); typ {
		case "assistant_message":
			text, _ := it.String("text")
			if !agyNotice(text) {
				return "", "", false
			}
			id, _ := it.String("messageId")
			return id, text, true
		case "reasoning":
			return "", "", false
		case "tool_call":
			if _, incoming := it.Get("agentMessage"); !incoming {
				return "", "", false
			}
		}
	}
	return "", "", false
}

// agyCacheFile holds the stop checks of agy agents, keyed by agent id. It
// is not a registration, so it must not end in .json.
func (e *Engine) agyCacheFile() string { return filepath.Join(e.StateDir(), "agy-stops.cache") }

// agyCacheKey changes whenever the agent does anything: a new turn, even
// one that only hits the limit again, moves lastActivityAt. Paseo closing
// an idle agent does not, so a stop seen while idle still counts once it is
// closed.
func agyCacheKey(record *Object) string {
	if at, ok := record.String("lastActivityAt"); ok && at != "" {
		return at
	}
	return pyStr(record.Value("updatedAt"))
}

// AgyStopOf reads the quota stop of an idle Antigravity agent of home from
// its Paseo timeline; the answer is cached until the agent's next activity.
// A closed agent gets only the cached answer: reading its timeline makes
// Paseo load it again, which starts agy (and fails for an old session).
func (e *Engine) AgyStopOf(record *Object, home string) (AgyStop, error) {
	if e.agyStopFn != nil {
		return e.agyStopFn(record, home)
	}
	agentID, _ := record.String("id")
	if agentID == "" || truthy(record.Value("archivedAt")) {
		return AgyStop{}, nil
	}
	key := agyCacheKey(record)
	if stop, ok := e.agyCached(agentID, key); ok {
		return stop, nil
	}
	tailer, ok := e.Paseo.(Tailer)
	if status, _ := record.String("lastStatus"); !ok || status != "idle" {
		return AgyStop{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), agyTailTimeout)
	items, err := tailer.Tail(ctx, agentID, home, agyTimelineTail)
	cancel()
	if err != nil {
		return AgyStop{}, err
	}
	stop := AgyStop{}
	if id, text, found := agyStopIn(items); found {
		stop.Failure = mustDigest([]any{"agy-limit", agentID, id, strings.TrimSpace(text)})
		if wait := agyResetIn(text); wait > 0 {
			if at := parseTime(record.Value("lastActivityAt")); at > 0 {
				stop.ResetAt = at + wait.Seconds()
			}
		}
	}
	e.agyRemember(agentID, key, stop)
	return stop, nil
}

// agyCached is the cached stop check of an agent, if its key still matches.
func (e *Engine) agyCached(agentID, key string) (AgyStop, bool) {
	e.agyMu.Lock()
	defer e.agyMu.Unlock()
	cache, _ := readObject(e.agyCacheFile())
	hit, ok := cache.Object(agentID)
	if !ok || pyStr(hit.Value("key")) != key {
		return AgyStop{}, false
	}
	stop := AgyStop{ResetAt: epochOf(hit.Value("reset_at"))}
	stop.Failure, _ = hit.String("failure")
	return stop, true
}

// agyRemember stores a stop check. Losing a write (another process wrote
// the file meanwhile) costs one more timeline read later.
func (e *Engine) agyRemember(agentID, key string, stop AgyStop) {
	e.agyMu.Lock()
	defer e.agyMu.Unlock()
	if _, err := privateDir(e.StateDir()); err != nil {
		return
	}
	path := e.agyCacheFile()
	cache, _ := readObject(path)
	if cache == nil {
		cache = NewObject()
	}
	entry := Obj("key", key, "failure", stop.Failure)
	if stop.ResetAt > 0 {
		entry.Set("reset_at", stop.ResetAt)
	}
	cache.Set(agentID, entry)
	_ = atomicWrite(path, cache)
}

// agyLimitFailure is limitFailure for an Antigravity agent.
func (e *Engine) agyLimitFailure(record *Object, home string) (string, error) {
	stop, err := e.AgyStopOf(record, home)
	return stop.Failure, err
}

// agyTranscript is agy's full log of the agent's conversation, or "".
func agyTranscript(record *Object, appDir string) string {
	persistence, _ := record.Value("persistence").(*Object)
	session, ok := parseUUID(persistence.Value("sessionId"))
	if !ok || appDir == "" {
		return ""
	}
	path := filepath.Join(appDir, "brain", session, ".system_generated", "logs", "transcript_full.jsonl")
	if m, _ := filepath.Glob(path); len(m) != 1 {
		return ""
	}
	return path
}

// agyPrompt is the prompt of an agy USER_INPUT entry, without the
// <USER_REQUEST> wrapper and the metadata agy appends.
func agyPrompt(content string) string {
	s := strings.TrimSpace(content)
	if rest, ok := strings.CutPrefix(s, "<USER_REQUEST>"); ok {
		s = rest
		if i := strings.Index(s, "</USER_REQUEST>"); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// scanAgy reads an agy transcript tail. Each model call is a
// PLANNER_RESPONSE with its uncached and cached input; a CHECKPOINT is a
// compaction (agy's own, or a /compact).
func scanAgy(lines [][]byte) Activity {
	a := Activity{Found: true}
	keeperTurn := false
	for _, raw := range lines {
		v, ok := decodeLine(raw)
		if !ok {
			continue
		}
		entry, ok := v.(*Object)
		if !ok {
			continue
		}
		at := parseTime(entry.Value("created_at"))
		typ, _ := entry.String("type")
		switch typ {
		case "USER_INPUT":
			content, _ := entry.String("content")
			keeperTurn = IsKeeperPrompt(agyPrompt(content))
			if !keeperTurn && at > a.RealAt {
				a.RealAt = at
			}
		case "PLANNER_RESPONSE":
			if at > a.CacheAt {
				a.CacheAt = at
			}
			if !keeperTurn && at > a.RealAt {
				a.RealAt = at
			}
			in, ok1 := numberOf(entry.Value("input_tokens"))
			cached, ok2 := numberOf(entry.Value("cache_read_tokens"))
			if ok1 || ok2 {
				a.ContextTokens, a.HasContext = in+cached, true
			}
		case "CHECKPOINT":
			a.CompactedAt = at
			if at > a.CacheAt {
				a.CacheAt = at
			}
			a.ContextTokens, a.HasContext = 0, false
		}
	}
	a.LastTurnKeeper = keeperTurn
	return a
}
