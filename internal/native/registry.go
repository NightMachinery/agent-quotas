package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The registry is byte-compatible with the shell's: one file per session at
// <dir>/<scope>/<agent>-<id>, holding one tab-separated line (agent, id,
// transcript, targets space-separated, registered-at epoch).
// Beside the registrations, dotfiles that the shell's globs skip:
//
//   - .blocked_at: epoch at which the scope was first seen blocked in the
//     current cycle; the activity gate's reference time.
//   - .blocked-<agent>-<id>: the session's own turn ended on the limit (left
//     by the hook); such a session is eligible whatever its transcript says.
//   - .kicked: the hook's debounce stamp (its mtime counts).
//   - .fn_ran: a transient stamp of the shell's arm; this engine only
//     removes it when a scope is emptied.
//   - .blocked-state: this engine's own addition, the groups that are blocked
//     in the current cycle with their reset times (JSON). Named .blocked-* so
//     the shell's "unblocked" cleanup removes it along with the marks.

const (
	blockedAtFile    = ".blocked_at"
	blockedStateFile = ".blocked-state"
	kickedFile       = ".kicked"
	fnRanFile        = ".fn_ran"
	markPrefix       = ".blocked-"
)

// Registration is one registered session.
type Registration struct {
	Scope        string   `json:"scope"`
	Agent        string   `json:"agent"`
	ID           string   `json:"id"`
	Transcript   string   `json:"transcript"`
	Targets      []string `json:"targets"`
	RegisteredAt int64    `json:"registered_at"`
}

// Key is the registration's file name, <agent>-<id>.
func (r Registration) Key() string { return r.Agent + "-" + r.ID }

// Line is the registration's file content, without the trailing newline.
// The targets keep their order: the shell's comment says "sorted", but its
// "${(j: :)${(o)targets}}" joins before it sorts, so it writes them in the
// order given, and so does this.
func (r Registration) Line() string {
	return strings.Join([]string{
		r.Agent, r.ID, r.Transcript, strings.Join(r.Targets, " "),
		strconv.FormatInt(r.RegisteredAt, 10),
	}, "\t")
}

// ParseRegistration reads one registration line as the shell writes it.
func ParseRegistration(scope, line string) (Registration, error) {
	line = strings.TrimRight(line, "\r\n")
	f := strings.Split(line, "\t")
	if len(f) < 2 || f[0] == "" || f[1] == "" {
		return Registration{}, fmt.Errorf("not a registration line: %q", line)
	}
	for len(f) < 5 {
		f = append(f, "")
	}
	r := Registration{Scope: scope, Agent: f[0], ID: f[1], Transcript: f[2]}
	r.Targets = strings.Fields(f[3])
	if v, err := strconv.ParseInt(strings.TrimSpace(f[4]), 10, 64); err == nil {
		r.RegisteredAt = v
	}
	return r, nil
}

func (e *Engine) scopeDir(scope string) string {
	return filepath.Join(e.dir(), scope)
}

func (e *Engine) regFile(scope, agent, id string) string {
	return filepath.Join(e.scopeDir(scope), agent+"-"+id)
}

func (e *Engine) markFile(scope, agent, id string) string {
	return filepath.Join(e.scopeDir(scope), markPrefix+agent+"-"+id)
}

// validName refuses path components that would escape the registry.
func validName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00\t\n") && !strings.HasPrefix(s, ".")
}

// Scopes lists every scope with at least one registration, sorted. A
// directory holding only dotfiles is not a scope anyone is waiting on.
func (e *Engine) Scopes() ([]string, error) {
	ents, err := os.ReadDir(e.dir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ent := range ents {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		regs, err := e.Registrations(ent.Name())
		if err == nil && len(regs) > 0 {
			out = append(out, ent.Name())
		}
	}
	return out, nil
}

// Registrations reads one scope's registrations, sorted by file name (the
// shell's glob order). Unreadable or malformed files are skipped.
func (e *Engine) Registrations(scope string) ([]Registration, error) {
	dir := e.scopeDir(scope)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Registration
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), ".") || !ent.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		r, err := ParseRegistration(scope, string(data))
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// FindRegistration looks for <agent>-<id> in every scope, as the hook's glob
// does, and returns the first match in scope order.
func (e *Engine) FindRegistration(agent, id string) (Registration, bool) {
	if !validName(agent) || !validName(id) {
		return Registration{}, false
	}
	ents, err := os.ReadDir(e.dir())
	if err != nil {
		return Registration{}, false
	}
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		p := e.regFile(ent.Name(), agent, id)
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if r, err := ParseRegistration(ent.Name(), string(data)); err == nil {
			return r, true
		}
	}
	return Registration{}, false
}

// writeRegistration stores r atomically (a dot-prefixed temporary file, so
// the shell's glob never sees a half-written one).
func (e *Engine) writeRegistration(r Registration) error {
	if !validName(r.Scope) || !validName(r.Agent) || !validName(r.ID) {
		return fmt.Errorf("invalid registration name: scope %q agent %q id %q", r.Scope, r.Agent, r.ID)
	}
	return writeFileAtomic(e.regFile(r.Scope, r.Agent, r.ID), []byte(r.Line()+"\n"))
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// blockedSince reads .blocked_at: the epoch the scope was first seen blocked
// in this cycle.
func (e *Engine) blockedSince(scope string) (int64, bool) {
	data, err := os.ReadFile(filepath.Join(e.scopeDir(scope), blockedAtFile))
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || strings.HasPrefix(s, "+") {
		return 0, false
	}
	return v, true
}

// blockedNow writes .blocked_at unless a cycle is already recorded.
func (e *Engine) blockedNow(scope string, now time.Time) (int64, error) {
	if v, ok := e.blockedSince(scope); ok {
		return v, nil
	}
	v := now.Unix()
	return v, writeFileAtomic(filepath.Join(e.scopeDir(scope), blockedAtFile), []byte(strconv.FormatInt(v, 10)+"\n"))
}

// unblocked ends the cycle: the reference time, the marks and the engine's
// state go, as the shell's h-agent-auto-continue-unblocked does.
func (e *Engine) unblocked(scope string) {
	dir := e.scopeDir(scope)
	_ = os.Remove(filepath.Join(dir, blockedAtFile))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), markPrefix) {
			_ = os.Remove(filepath.Join(dir, ent.Name()))
		}
	}
}

func (e *Engine) marked(scope, agent, id string) bool {
	_, err := os.Stat(e.markFile(scope, agent, id))
	return err == nil
}

// blockedGroup is one thing that blocked the scope in the current cycle: the
// whole account ("*") or one Claude model family ("fam:<family>").
type blockedGroup struct {
	// ResetsAt is the latest reset seen for the group, epoch seconds, 0 when
	// unknown.
	ResetsAt float64 `json:"resets_at"`
}

type scopeState struct {
	Groups map[string]blockedGroup `json:"groups"`
}

const allGroup = "*"

func famGroup(f string) string { return "fam:" + f }

func (e *Engine) readState(scope string) scopeState {
	st := scopeState{Groups: map[string]blockedGroup{}}
	data, err := os.ReadFile(filepath.Join(e.scopeDir(scope), blockedStateFile))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	if st.Groups == nil {
		st.Groups = map[string]blockedGroup{}
	}
	return st
}

func (e *Engine) writeState(scope string, st scopeState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(e.scopeDir(scope), blockedStateFile), append(data, '\n'))
}
