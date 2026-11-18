package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// ScopeQuota is what the daemon knows about a scope's account when it asks
// for a check.
type ScopeQuota struct {
	// Verdict is the account verdict (model.Usable, Blocked, Unknown,
	// SignedOut). Blocked means every session of the scope is blocked.
	Verdict string
	// BlockedUntil is the latest reset among the account-wide blocking
	// windows, epoch seconds, or nil.
	BlockedUntil *float64
	// ModelBlocks are the model-scoped windows that block now (Claude). A
	// registration on model family f is blocked by one whose Name contains
	// f, case-insensitively.
	ModelBlocks []ModelBlock
}

// ModelBlock is one blocking model-scoped window.
type ModelBlock struct {
	Name     string   `json:"name"`
	ResetsAt *float64 `json:"resets_at,omitempty"`
}

// QuotaFromSnapshot builds a ScopeQuota from an account snapshot: the
// verdict and blocked_until as they are, and every blocking window with
// scope "model" as a ModelBlock named by its family and label.
func QuotaFromSnapshot(s model.Snapshot) ScopeQuota {
	q := ScopeQuota{Verdict: s.Verdict, BlockedUntil: s.BlockedUntil}
	for _, w := range s.Windows {
		if w.Blocking && w.Scope == model.ScopeModel {
			q.ModelBlocks = append(q.ModelBlocks, ModelBlock{Name: strings.TrimSpace(w.Family + " " + w.Label + " " + w.Key), ResetsAt: w.ResetsAt})
		}
	}
	return q
}

// familyBlocked reports whether family f is blocked by a model window now,
// and that window's reset.
func (q ScopeQuota) familyBlocked(f string) (bool, float64) {
	if f == "" {
		return false, 0
	}
	lf := strings.ToLower(f)
	for _, b := range q.ModelBlocks {
		if strings.Contains(strings.ToLower(b.Name), lf) {
			r := 0.0
			if b.ResetsAt != nil {
				r = *b.ResetsAt
			}
			return true, r
		}
	}
	return false, 0
}

// Check outcomes.
const (
	OutcomeEmpty   = "empty"   // nothing registered (or everything pruned)
	OutcomeUnknown = "unknown" // the account's state cannot decide anything
	OutcomeBlocked = "blocked" // blocked; the reference time is recorded
	OutcomeIdle    = "idle"    // usable and no cycle in progress
	OutcomeWaiting = "waiting" // usable again, but the grace has not passed
	OutcomeResumed = "resumed" // something was resumed (or tried)
)

// Decision is what one check decided, for the daemon to log and report.
type Decision struct {
	Scope        string       `json:"scope"`
	Account      string       `json:"account"`
	Outcome      string       `json:"outcome"`
	Kept         []string     `json:"kept,omitempty"`   // <agent>-<id>
	Pruned       []string     `json:"pruned,omitempty"` // <agent>-<id>, no longer live
	BlockedSince *int64       `json:"blocked_since,omitempty"`
	Skipped      []string     `json:"skipped,omitempty"` // not working when the limit hit
	Fires        []FireReport `json:"fires,omitempty"`
	// RetryAt asks the daemon to check again at this epoch (the end of the
	// grace after a reset).
	RetryAt *float64 `json:"retry_at,omitempty"`
	// Lines are the human messages the shell printed in gray.
	Lines []string `json:"lines,omitempty"`
}

// Check runs one check of scope against the account state q: prune
// registrations whose session ended, record the start of a blocked cycle,
// and once the account (or a Claude family) that blocked a session is usable
// again and the grace has passed, resume the sessions that were still
// working when the limit hit. Safe to call any number of times.
func (e *Engine) Check(ctx context.Context, scope string, q ScopeQuota) (Decision, error) {
	l := e.scopeLock(scope)
	l.Lock()
	defer l.Unlock()

	d := Decision{Scope: scope, Account: e.AccountForScope(scope)}
	if !validName(scope) {
		return d, fmt.Errorf("invalid scope %q", scope)
	}
	regs, err := e.Registrations(scope)
	if err != nil {
		return d, err
	}
	if len(regs) == 0 {
		d.Outcome = OutcomeEmpty
		return d, nil
	}

	// Prune against one live listing. A listing that failed prunes nothing:
	// "cannot tell" must not forget every registration.
	if live, err := e.Lister.Live(ctx); err == nil {
		var kept []Registration
		for _, r := range regs {
			if isLive(live, r.ID, r.Transcript) {
				kept = append(kept, r)
				continue
			}
			if err := os.Remove(e.regFile(scope, r.Agent, r.ID)); err == nil || os.IsNotExist(err) {
				d.Pruned = append(d.Pruned, r.Key())
				d.Lines = append(d.Lines, fmt.Sprintf("%s: %s is no longer live, forgetting it", scope, r.Key()))
			} else {
				kept = append(kept, r)
			}
		}
		regs = kept
	} else {
		d.Lines = append(d.Lines, fmt.Sprintf("%s: live listing failed, not pruning: %v", scope, err))
	}
	for _, r := range regs {
		d.Kept = append(d.Kept, r.Key())
	}
	if len(regs) == 0 {
		d.Outcome = OutcomeEmpty
		return d, nil
	}

	if q.Verdict != model.Usable && q.Verdict != model.Blocked {
		d.Outcome = OutcomeUnknown
		if v, ok := e.blockedSince(scope); ok {
			d.BlockedSince = &v
		}
		return d, nil
	}

	now := e.now()
	nowE := float64(now.Unix())
	isClaude := ScopeAgent(scope) == "claude"
	family := map[string]string{}
	if isClaude {
		for _, r := range regs {
			if f, err := ClaudeFamily(r.Transcript); err == nil {
				family[r.Key()] = f
			}
		}
	}

	// What blocks now.
	blockedNowGroups := map[string]blockedGroup{}
	if q.Verdict == model.Blocked {
		g := blockedGroup{}
		if q.BlockedUntil != nil {
			g.ResetsAt = *q.BlockedUntil
		}
		blockedNowGroups[allGroup] = g
	}
	if isClaude {
		for _, f := range family {
			if ok, r := q.familyBlocked(f); ok {
				blockedNowGroups[famGroup(f)] = blockedGroup{ResetsAt: r}
			}
		}
	}
	regBlockedNow := func(r Registration) bool {
		if _, ok := blockedNowGroups[allGroup]; ok {
			return true
		}
		f := family[r.Key()]
		_, ok := blockedNowGroups[famGroup(f)]
		return f != "" && ok
	}

	prev := e.readState(scope)
	_, hadCycle := e.blockedSince(scope)
	if hadCycle && len(prev.Groups) == 0 {
		// A cycle recorded by the shell watcher, which kept no groups: it
		// blocked the whole scope as far as this engine can tell.
		prev.Groups[allGroup] = blockedGroup{}
	}

	if len(blockedNowGroups) > 0 {
		ref, err := e.blockedNow(scope, now)
		if err != nil {
			return d, err
		}
		d.BlockedSince = &ref
	}

	// Groups that blocked earlier in this cycle and no longer do.
	grace := float64(e.Cfg.Native.GraceS)
	due := map[string]bool{}
	deferred := map[string]blockedGroup{}
	var retry float64
	for k, g := range prev.Groups {
		if _, still := blockedNowGroups[k]; still {
			continue
		}
		if g.ResetsAt > 0 && nowE < g.ResetsAt+grace {
			deferred[k] = g
			if retry == 0 || g.ResetsAt+grace < retry {
				retry = g.ResetsAt + grace
			}
			continue
		}
		due[k] = true
	}

	// The registrations to resume: blocked earlier, not blocked now, and
	// every group that blocked them is due.
	ref, haveRef := e.blockedSince(scope)
	pollSlack := e.providerMinInterval(scope) + int64(e.Cfg.Native.ActivitySlackS)
	groupsOf := func(r Registration) []string {
		var gs []string
		if _, ok := prev.Groups[allGroup]; ok {
			gs = append(gs, allGroup)
		}
		if f := family[r.Key()]; f != "" {
			if _, ok := prev.Groups[famGroup(f)]; ok {
				gs = append(gs, famGroup(f))
			}
		}
		return gs
	}
	byLabel := map[string][]Registration{}
	var labels []string
	for _, r := range regs {
		gs := groupsOf(r)
		if len(gs) == 0 || regBlockedNow(r) {
			continue
		}
		allDue := true
		for _, g := range gs {
			if !due[g] {
				allDue = false
			}
		}
		if !allDue {
			continue
		}
		if haveRef && !e.eligible(scope, r, ref, pollSlack) {
			d.Skipped = append(d.Skipped, r.Key())
			d.Lines = append(d.Lines, fmt.Sprintf("%s: %s %s was not working when the limit hit, not resuming it", scope, r.Agent, r.ID))
			_ = os.Remove(e.markFile(scope, r.Agent, r.ID))
			continue
		}
		label := e.jobLabel(scope, family[r.Key()])
		if _, ok := byLabel[label]; !ok {
			labels = append(labels, label)
		}
		byLabel[label] = append(byLabel[label], r)
	}
	sort.Strings(labels)

	for _, label := range labels {
		var targets []string
		var fams []string
		for _, r := range byLabel[label] {
			targets = append(targets, r.Targets...)
			fams = append(fams, family[r.Key()])
		}
		msg := e.resumeMessage(scope, due, sortedUnique(fams))
		rep := e.Fire(ctx, label, msg, sortedUnique(targets))
		d.Fires = append(d.Fires, rep)
		for _, r := range byLabel[label] {
			_ = os.Remove(e.markFile(scope, r.Agent, r.ID))
		}
	}

	// The next state: what blocks now, plus the recovered groups still in
	// their grace.
	next := scopeState{Groups: map[string]blockedGroup{}}
	for k, g := range blockedNowGroups {
		if p, ok := prev.Groups[k]; ok && p.ResetsAt > g.ResetsAt {
			g.ResetsAt = p.ResetsAt
		}
		next.Groups[k] = g
	}
	for k, g := range deferred {
		next.Groups[k] = g
	}
	if len(next.Groups) == 0 {
		if hadCycle || len(prev.Groups) > 0 {
			e.unblocked(scope)
		}
	} else if err := e.writeState(scope, next); err != nil {
		return d, err
	}
	if retry > 0 {
		d.RetryAt = &retry
	}

	switch {
	case len(d.Fires) > 0:
		d.Outcome = OutcomeResumed
	case len(blockedNowGroups) > 0:
		d.Outcome = OutcomeBlocked
	case len(deferred) > 0:
		d.Outcome = OutcomeWaiting
	default:
		d.Outcome = OutcomeIdle
	}
	if len(next.Groups) > 0 {
		if v, ok := e.blockedSince(scope); ok {
			d.BlockedSince = &v
		}
	} else {
		d.BlockedSince = nil
	}
	return d, nil
}

// jobLabel is the old armed job's session name, which the log and the
// notifications use: agent-auto-continue-<scope>[-<family>].
func (e *Engine) jobLabel(scope, family string) string {
	s := "agent-auto-continue-" + scope
	if family != "" {
		s += "-" + family
	}
	return strings.NewReplacer(".", "-", ":", "-").Replace(s)
}

func (e *Engine) resumeMessage(scope string, due map[string]bool, fams []string) string {
	what := "quota"
	if !due[allGroup] {
		var parts []string
		for _, f := range fams {
			if f != "" && due[famGroup(f)] {
				parts = append(parts, f+" weekly")
			}
		}
		if len(parts) > 0 {
			what = strings.Join(parts, ", ")
		}
	}
	switch ScopeAgent(scope) {
	case "claude":
		return fmt.Sprintf("Claude Code (%s): %s reset, usage available again", strings.TrimPrefix(scope, "claude-"), what)
	case "codex":
		return fmt.Sprintf("Codex (%s): %s reset, usage available again", strings.TrimPrefix(scope, "codex-"), what)
	case "agy":
		return fmt.Sprintf("Antigravity: %s reset, usage available again", what)
	}
	return fmt.Sprintf("%s: %s reset, usage available again", scope, what)
}

// eligible is h-agent-auto-continue-eligible-p: a hook mark, or a transcript
// modified within poll + slack before the reference time, or no transcript
// to ask ("cannot tell" is not "finished").
func (e *Engine) eligible(scope string, r Registration, ref, pollSlack int64) bool {
	if e.marked(scope, r.Agent, r.ID) {
		return true
	}
	if r.Transcript == "" {
		return true
	}
	st, err := os.Stat(r.Transcript)
	if err != nil {
		return true
	}
	return st.ModTime().Unix() >= ref-pollSlack
}

// ClaudeFamily is h-agent-auto-continue-claude-family: the model family
// (fable, opus, sonnet) named on the transcript's most recent assistant
// record, read from the last 200 lines only.
func ClaudeFamily(transcript string) (string, error) {
	if transcript == "" {
		return "", fmt.Errorf("no transcript")
	}
	lines, err := tailLines(transcript, 200, 64<<20)
	if err != nil {
		return "", err
	}
	modelID := ""
	for _, line := range lines {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.Message.Model == "" {
			continue
		}
		modelID = rec.Message.Model
	}
	if modelID == "" {
		return "", fmt.Errorf("no assistant model in %s", filepath.Base(transcript))
	}
	m := strings.TrimPrefix(modelID, "claude-")
	f, _, _ := strings.Cut(m, "-")
	return f, nil
}

// tailLines returns the last n lines of a file, reading backwards in chunks
// and never more than max bytes.
func tailLines(path string, n int, max int64) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	const chunk = 64 << 10
	var buf []byte
	pos := size
	for pos > 0 && size-pos < max {
		step := int64(chunk)
		if pos < step {
			step = pos
		}
		pos -= step
		b := make([]byte, step)
		if _, err := f.ReadAt(b, pos); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(b, buf...)
		if bytes.Count(buf, []byte{'\n'}) > n {
			break
		}
	}
	all := bytes.Split(bytes.TrimRight(buf, "\n"), []byte{'\n'})
	if pos > 0 && len(all) > 0 {
		all = all[1:] // a partial first line
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}
