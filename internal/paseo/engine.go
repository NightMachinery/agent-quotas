// Package paseo is the Paseo auto-continue engine, a port of the old
// paseo_auto_continue.py: registrations of Paseo agents that stopped on a
// usage limit, and the tick that sends one "Continue." after that agent's own
// account has quota again. See docs/design.md, "Paseo engine".
//
// Registration files, their locks and the failure digests are byte
// compatible with the Python tool, so the old workers and this engine can run
// side by side during the cutover without sending twice.
package paseo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/NightMachinery/agent-quotas/internal/config"
)

// Quotas answers "is this seat usable now" for the engine. The daemon
// implements it from its store, refreshing a reading that is too old before
// answering. An error means unknown: a tick postpones, a listing warns.
type Quotas interface {
	// Claude reports whether the Claude profile has room for the model
	// family ("opus", "sonnet", ...; "" for none): the account-wide windows
	// and that family's scoped weekly window are below 100%.
	Claude(ctx context.Context, profile, family string) (bool, error)
	// Codex reports the regular-allowance verdict of the active auth of
	// this CODEX_HOME (not of whichever other account has room).
	Codex(ctx context.Context, codexHome string) (bool, error)
	// Agy reports whether every quota pool that limits model on this
	// Antigravity account has room (agy-best's rule).
	Agy(ctx context.Context, account, model string) (bool, error)
}

// Status strings a tick reports, identical to the Python tool's.
const (
	StatusOff            = "off"
	StatusUnavailable    = "agent unavailable"
	StatusProviderChange = "provider changed; re-arm explicitly"
	StatusWaitingFailure = "waiting for a quota-failed turn"
	StatusWaitingNew     = "waiting for a new quota-failed turn"
	StatusWaitingQuota   = "waiting for quota recovery"
	StatusChanged        = "agent changed during quota check; skipped"
	StatusSending        = "sending continuation"
	StatusUncertain      = "send uncertain; inspect agent and re-arm explicitly"
	StatusSent           = "continuation sent; waiting for a new quota-failed turn"
	// StatusPingNotContinued: the failed turn was a keeper ping; it is
	// claimed without a send.
	StatusPingNotContinued = "keeper ping failed on quota; not continued"
	// StatusCheckUnavailable is what the old worker printed when a tick
	// failed; the daemon may log it.
	StatusCheckUnavailable = "Check unavailable; no message sent"
)

// Engine runs Paseo registrations. Its zero value is not usable; use New.
type Engine struct {
	Config *config.Config
	Paseo  Paseo
	Quotas Quotas
	// UserHome is what ~ means; "" means config.Home().
	UserHome string
	// Getenv reads the environment (PASEO_HOME, PASEO_HOST, the send
	// behaviour variables); nil means os.Getenv.
	Getenv func(string) string
	// Now is the clock for registered_at; nil means time.Now.
	Now func() time.Time

	// Test seams, the Python tool's injectable functions.
	storedFn     func(agentID, home string) (*Object, error)
	transcriptFn func(record *Object, id Identity) (string, error)
	checkableFn  func(provider any) bool
	agyAccountFn func(provider any) (string, bool, error)
	activityFn   func(record *Object, id Identity) (Activity, error)
}

// New returns an engine using the paseo command found by cfg.Paseo.Binary.
func New(cfg *config.Config, quotas Quotas) *Engine {
	return &Engine{Config: cfg, Quotas: quotas, Paseo: CLI{Binary: cfg.Paseo.Binary}}
}

func (e *Engine) userHome() string {
	if e.UserHome != "" {
		return e.UserHome
	}
	return config.Home()
}

func (e *Engine) getenv(name string) string {
	if e.Getenv != nil {
		return e.Getenv(name)
	}
	return os.Getenv(name)
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// StateDir is the registrations directory (cfg.Paseo.RegistrationsDir).
func (e *Engine) StateDir() string {
	dir := ""
	if e.Config != nil {
		dir = e.Config.Paseo.RegistrationsDir
	}
	if dir == "" {
		return filepath.Join(config.AgentSessionsDir(), "auto-continue-paseo")
	}
	return expandUser(dir, e.userHome())
}

// ResolveHome resolves a Paseo daemon home as the old CLI did: "" means
// PASEO_HOME, else the first configured home (~/.paseo); then ~ expansion
// and symlink resolution.
func (e *Engine) ResolveHome(home string) string {
	if home == "" {
		home = e.getenv("PASEO_HOME")
	}
	if home == "" {
		home = "~/.paseo"
		if e.Config != nil && len(e.Config.Paseo.Homes) > 0 {
			home = e.Config.Paseo.Homes[0]
		}
	}
	return realpath(expandUser(home, e.userHome()))
}

// FileName is the registration file name of an agent under a resolved home:
// sha256(json.dumps(home))[:16] + "-" + agent + ".json".
func FileName(home, agentID string) string {
	return mustDigest(home)[:16] + "-" + agentID + ".json"
}

func (e *Engine) stored(agentID, home string) (*Object, error) {
	if e.storedFn != nil {
		return e.storedFn(agentID, home)
	}
	return storedAgent(agentID, home)
}

func (e *Engine) transcript(record *Object, id Identity) (string, error) {
	if e.transcriptFn != nil {
		return e.transcriptFn(record, id)
	}
	return limitFailure(record, id)
}

// storedAgent ports stored_agent: the persisted record of one agent.
func storedAgent(agentID, home string) (*Object, error) {
	agents := filepath.Join(home, "agents")
	m1, _ := filepath.Glob(filepath.Join(agents, agentID+".json"))
	m2, _ := filepath.Glob(filepath.Join(agents, "*", agentID+".json"))
	matches := append(m1, m2...)
	if len(matches) != 1 {
		return nil, refuse("Cannot find one local persisted Paseo agent")
	}
	record, err := readAgentRecord(matches[0], os.ReadFile)
	if err != nil {
		return nil, err
	}
	if id, _ := record.String("id"); id != agentID {
		return nil, refuse("Stored agent ID does not match")
	}
	return record, nil
}

// readAgentRecord retries decoding once if Paseo was in the middle of a write.
func readAgentRecord(path string, read func(string) ([]byte, error)) (*Object, error) {
	for attempt := 0; ; attempt++ {
		data, err := read(path)
		if err != nil {
			return nil, err
		}
		record, err := DecodeObject(data)
		if err == nil || attempt == 1 {
			return record, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// StoredStatus is the lifecycle status in a persisted Paseo agent record.
func StoredStatus(record *Object) string {
	status, _ := record.String("lastStatus")
	switch status {
	case "idle", "closed", "error", "running", "initializing":
		return status
	default:
		return ""
	}
}

// busyStatus recognizes live or persisted states in which no resume is due.
func busyStatus(status string) bool {
	return status == "running" || status == "initializing"
}

// PollListing groups read-only fallback queries for a scheduler round.
// It is lazy so a round answered entirely by files never starts the CLI.
type PollListing struct {
	once sync.Once
	byID map[string]string
	err  error
}

type agentLister interface {
	List(context.Context, string) ([]*Object, error)
}

// Status returns the live status of an exact ID in the round's listing.
// A missing row is inconclusive because the listing omits archived and
// background agents.
func (b *PollListing) Status(ctx context.Context, cli agentLister, home, agentID string) (string, error) {
	b.once.Do(func() {
		items, err := cli.List(ctx, home)
		b.err = err
		b.byID = make(map[string]string, len(items))
		for _, item := range items {
			id, _ := item.String("id")
			status, _ := item.String("status")
			b.byID[id] = status
		}
	})
	return b.byID[agentID], b.err
}

// pollStatus skips routine live checks when the file establishes there is
// no new failed turn. Archival and binding changes still go through the
// live check before changing a registration.
func (e *Engine) pollStatus(ctx context.Context, batch *PollListing, state, binding *Object, home, agentID string) (string, error) {
	record, err := e.stored(agentID, home)
	status := StoredStatus(record)
	if err != nil || status == "" {
		if cli, ok := e.Paseo.(agentLister); ok {
			status, err := batch.Status(ctx, cli, home, agentID)
			if errors.Is(err, ErrCommandUnavailable) {
				return "", err
			}
			if busyStatus(status) {
				return StatusWaitingFailure, nil
			}
		}
		return "", nil
	}
	if truthy(record.Value("archivedAt")) {
		return "", nil
	}
	provider, gotProvider := record.String("provider")
	cwd, gotCwd := record.String("cwd")
	if !gotProvider || !gotCwd {
		return "", nil
	}
	current, err := e.binding(Obj("Provider", provider, "Cwd", cwd), home)
	if err != nil || !equal(current, binding) {
		return "", nil
	}

	if busyStatus(status) {
		return StatusWaitingFailure, nil
	}
	if status != "idle" && status != "closed" && status != "error" {
		return "", nil
	}
	failure, err := e.failureOf(Obj("Status", status), record, binding)
	if err != nil {
		return "", nil // live status may make a transcript check unnecessary
	}
	if failure == "" {
		if status != "error" {
			return StatusWaitingFailure, nil
		}
		// A collected error may appear closed in Paseo. Check the transcript
		// too before ruling out a failure when the record has no quota error.
		if failure, err := e.failureOf(Obj("Status", "closed"), record, binding); err != nil || failure != "" {
			return "", nil
		}
	}
	claimed, _ := state.String("claimed_failure")
	if failure == "" || failure == claimed {
		return StatusWaitingNew, nil
	}
	return "", nil
}

// failureOf ports failure_of: the current failed turn's identity, or "".
func (e *Engine) failureOf(snapshot, record *Object, binding *Object) (string, error) {
	status, _ := snapshot.String("Status")
	// Idle runtime cleanup reports the persisted agent as closed. Its last
	// quota failure still counts; loading the runtime is not a new turn.
	if status == "error" || status == "closed" {
		failure := quotaFailure(record)
		if failure != "" || status == "error" {
			return failure, nil
		}
	}
	id := identityOf(binding)
	if (status == "idle" || status == "closed") && (id.Agent == "claude" || id.Agent == "codex") &&
		!truthy(record.Value("archivedAt")) {
		return e.transcript(record, id)
	}
	return "", nil
}

// pingMatchSlack is how close a keeper ping's send time and Paseo's
// lastUserMessageAt must be for the message to be that ping.
const pingMatchSlack = 15.0

// Activity reads an agent's transcript activity (see ActivityOf).
func (e *Engine) Activity(record *Object, id Identity) (Activity, error) {
	if e.activityFn != nil {
		return e.activityFn(record, id)
	}
	return ActivityOf(record, id)
}

// keeperTurnFailed reports whether the failed last turn was started by a
// keeper ping: from the transcript, or, without one, from the recorded ping
// time matching Paseo's lastUserMessageAt.
func (e *Engine) keeperTurnFailed(state, record, binding *Object) bool {
	if act, err := e.Activity(record, identityOf(binding)); err == nil && act.Found && act.LastTurnKeeper {
		return true
	}
	cache, _ := state.Object("cache")
	ping, _ := cache.Object("last_ping")
	at, ok := numberOf(ping.Value("at"))
	user := parseTime(record.Value("lastUserMessageAt"))
	return ok && at > 0 && user > 0 && user >= float64(at)-pingMatchSlack && user <= float64(at)+pingMatchSlack
}

// Stored is the persisted Paseo record of an agent under home.
func (e *Engine) Stored(agentID, home string) (*Object, error) { return e.stored(agentID, home) }

// Binding is the binding of an agent from its `paseo inspect` output.
func (e *Engine) Binding(snapshot *Object, home string) (*Object, error) {
	return e.binding(snapshot, home)
}

// IdentityOf reads the identity back from a stored binding.
func IdentityOf(binding *Object) Identity { return identityOf(binding) }

// Failure is the identity of the agent's current quota-failed turn, or "".
func (e *Engine) Failure(snapshot, record, binding *Object) (string, error) {
	return e.failureOf(snapshot, record, binding)
}

// quotaFor ports quota(): ask the daemon about the seat this provider
// resolves to now, for the model's family.
func (e *Engine) quotaFor(ctx context.Context, home string, provider any, model string) (bool, error) {
	id, err := e.providerConfig(home, provider)
	if err != nil {
		return false, err
	}
	if e.Quotas == nil {
		return false, errors.New("no quota source")
	}
	if id.Agent == "codex" {
		return e.Quotas.Codex(ctx, id.NativeHome)
	}
	return e.Quotas.Claude(ctx, id.Profile, ModelFamily(model))
}

// Tick runs one check of the registration at path and returns its status.
// It records the claim before sending, so a failed turn gets at most one
// automatic send, across restarts and alongside the old Python workers. An
// error leaves the registration as it was, except after a send failure,
// which disables it (the send may have happened).
func (e *Engine) Tick(ctx context.Context, path string) (string, error) {
	return e.tick(ctx, path, nil)
}

// Poll checks a registration with a file-first screen and a per-home listing
// shared by the registrations due in this round.
func (e *Engine) Poll(ctx context.Context, path string, batch *PollListing) (string, error) {
	return e.tick(ctx, path, batch)
}

func (e *Engine) tick(ctx context.Context, path string, batch *PollListing) (string, error) {
	unlock, err := lockFile(lockPath(path))
	if err != nil {
		return "", err
	}
	defer unlock()
	state, err := readObject(path)
	if err != nil {
		return "", err
	}
	enabled, ok := state.Get("enabled")
	if !ok {
		return "", errors.New("registration lacks enabled")
	}
	if !truthy(enabled) {
		return StatusOff, nil
	}
	home, ok1 := state.String("home")
	agentID, ok2 := state.String("agent_id")
	if !ok1 || !ok2 {
		return "", errors.New("registration lacks home or agent_id")
	}
	stored, _ := state.Object("binding")
	if stored == nil {
		stored = NewObject()
	}
	setStatus := func(s string) { state.Set("status", s) }
	if batch != nil {
		status, err := e.pollStatus(ctx, batch, state, stored, home, agentID)
		if err != nil {
			return "", err
		}
		if status != "" {
			if previous, _ := state.String("status"); previous == status {
				return status, nil
			}
			setStatus(status)
			return status, atomicWrite(path, state)
		}
	}
	snapshot, err := e.Paseo.Inspect(ctx, agentID, home)
	if err != nil {
		return "", err
	}
	if !equal(snapshot.Value("Id"), agentID) || truthy(snapshot.Value("Archived")) {
		state.Set("enabled", false)
		setStatus(StatusUnavailable)
	} else if b, err := e.binding(snapshot, home); err != nil {
		return "", err
	} else if !equal(b, state.Value("binding")) {
		state.Set("enabled", false)
		setStatus(StatusProviderChange)
	} else {
		record, err := e.stored(agentID, home)
		if err != nil {
			return "", err
		}
		failure, err := e.failureOf(snapshot, record, stored)
		if err != nil {
			return "", err
		}
		claimed, _ := state.String("claimed_failure")
		status, _ := snapshot.String("Status")
		model, _ := snapshot.String("Model")
		switch {
		case truthy(snapshot.Value("PendingPermissions")) || (failure == "" && status != "error"):
			setStatus(StatusWaitingFailure)
		case failure == "" || failure == claimed:
			setStatus(StatusWaitingNew)
		case e.keeperTurnFailed(state, record, stored):
			// A keepalive ping that hit the limit is not work to resume.
			state.Set("claimed_failure", failure)
			setStatus(StatusPingNotContinued)
		default:
			usable, err := e.quotaFor(ctx, home, stored.Value("provider"), model)
			if err != nil {
				return "", err
			}
			if !usable {
				setStatus(StatusWaitingQuota)
				break
			}
			// Recheck after a potentially slow account query: the user may
			// have continued, archived or changed profiles meanwhile.
			fresh, err := e.Paseo.Inspect(ctx, agentID, home)
			if err != nil {
				return "", err
			}
			current, err := e.stored(agentID, home)
			if err != nil {
				return "", err
			}
			changed := !equal(fresh.Value("Id"), agentID) || truthy(fresh.Value("Archived")) ||
				!equal(fresh.Value("Status"), snapshot.Value("Status")) || truthy(fresh.Value("PendingPermissions"))
			if !changed {
				fb, err := e.binding(fresh, home)
				if err != nil {
					return "", err
				}
				changed = !equal(fb, state.Value("binding"))
			}
			if !changed {
				again, err := e.failureOf(fresh, current, stored)
				if err != nil {
					return "", err
				}
				changed = again != failure || e.keeperTurnFailed(state, current, stored)
			}
			if changed {
				setStatus(StatusChanged)
				break
			}
			state.Set("claimed_failure", failure)
			setStatus(StatusSending)
			if err := atomicWrite(path, state); err != nil {
				return "", err
			}
			behavior := ""
			if e.Config != nil {
				behavior = e.Config.Paseo.SendBehavior
			}
			if err := e.Paseo.Send(ctx, agentID, home, ContinueText, SendBehavior(behavior, e.getenv)); err != nil {
				// A timeout can mean Paseo accepted the send. Never retry
				// that failed turn automatically; explicit re-arm clears it.
				state.Set("enabled", false)
				setStatus(StatusUncertain)
				if werr := atomicWrite(path, state); werr != nil {
					return StatusUncertain, errors.Join(err, werr)
				}
				return StatusUncertain, err
			}
			setStatus(StatusSent)
		}
	}
	if err := atomicWrite(path, state); err != nil {
		return "", err
	}
	status, _ := state.String("status")
	return status, nil
}

// CheckRemote refuses a remote Paseo host (PASEO_HOST set): registrations
// work only on the daemon's own host.
func CheckRemote(paseoHost string) error {
	if paseoHost != "" {
		return refuse("remote hosts are unsupported; run on the daemon host with --home")
	}
	return nil
}

// validate is the old CLI's argument checks; it returns the canonical id.
func (e *Engine) validate(agentID string, poll int) (string, error) {
	if agentID == "" {
		return "", refuse("pass an agent ID or run inside a Paseo agent")
	}
	id, ok := parseUUID(agentID)
	if !ok {
		return "", refuse("an exact agent UUID is required")
	}
	if poll < 30 {
		return "", refuse("--poll must be at least 30 seconds")
	}
	if err := CheckRemote(e.getenv("PASEO_HOST")); err != nil {
		return "", err
	}
	return id, nil
}

// RegistrationPath validates the agent id and returns the registration file
// of that agent under home ("" means the default home), creating the private
// registrations directory if needed.
func (e *Engine) RegistrationPath(agentID, home string) (string, error) {
	id, err := e.validate(agentID, 300)
	if err != nil {
		return "", err
	}
	return e.registrationPath(id, e.ResolveHome(home))
}

func (e *Engine) registrationPath(agentID, home string) (string, error) {
	dir, err := privateDir(e.StateDir())
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName(home, agentID)), nil
}

// ContinueDecided reports whether the registration has a prior decision.
func ContinueDecided(state *Object) bool {
	if state == nil {
		return false
	}
	// Older files did not distinguish an explicit Off from a keeper-only
	// registration. Treat them as decided so an upgrade cannot undo an opt-out,
	// except a file the keeper created to hold a cache policy: it was
	// registered in the same moment its cache policy was set.
	source, hasSource := state.Get("set_by")
	if !hasSource && !truthy(state.Value("enabled")) && keeperCreated(state) {
		return false
	}
	return !hasSource || source != nil || truthy(state.Value("enabled"))
}

// ContinueExplicit reports whether the registration holds a decision made
// for this agent itself: decided, and not by a default.
func ContinueExplicit(state *Object) bool {
	if !ContinueDecided(state) {
		return false
	}
	source, _ := state.String("set_by")
	return source != "default"
}

// DefaultMode is how a default decision treats the decision already
// recorded.
type DefaultMode int

const (
	// DefaultOnce decides only where nothing is decided yet (the keeper's own
	// default for main agents).
	DefaultOnce DefaultMode = iota
	// DefaultFollow also replaces an earlier default, so a default can follow
	// a setting that changed; a decision made for the agent itself stands.
	DefaultFollow
	// DefaultReset replaces any decision: the agent follows the default again.
	DefaultReset
)

// defaultMay reports whether a default decision for enabled may replace
// state; one that would change nothing is not written.
func defaultMay(state *Object, enabled bool, mode DefaultMode) bool {
	if source, _ := state.String("set_by"); source == "default" && truthy(state.Value("enabled")) == enabled {
		return false
	}
	switch mode {
	case DefaultReset:
		return true
	case DefaultFollow:
		return !ContinueExplicit(state)
	}
	return !ContinueDecided(state)
}

// keeperCreatedSlack is how far apart registered_at and the cache policy's
// set_at may be for a file the keeper created with the policy.
const keeperCreatedSlack = 5.0

func keeperCreated(state *Object) bool {
	cache, ok := state.Object("cache")
	if !ok {
		return false
	}
	registered, setAt := epochOf(state.Value("registered_at")), epochOf(cache.Value("set_at"))
	if registered <= 0 || setAt <= 0 {
		return false
	}
	d := setAt - registered
	return d >= -keeperCreatedSlack && d <= keeperCreatedSlack
}

// epochOf reads a Unix time stored as a number (json.Number from a file,
// float64 or int in memory); anything else is 0.
func epochOf(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0
		}
		return f
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

// On registers an agent (exact UUID; poll >= 30 s; local daemon only;
// archived agents refused). Repeating it for an enabled registration with
// the same binding keeps the claimed failure.
func (e *Engine) On(ctx context.Context, agentID, home string, poll int) (string, error) {
	out, _, err := e.enable(ctx, agentID, home, poll, "cli", DefaultOnce)
	return out, err
}

// Off disables a registration under the tick lock, so no send is in flight
// when it returns.
func (e *Engine) Off(_ context.Context, agentID, home string) (string, error) {
	out, _, err := e.disable(agentID, home, "cli", DefaultOnce)
	return out, err
}

// SetAutoContinue uses the CLI registration path for an API or default
// decision; mode says what a default may replace (see DefaultMode). It
// returns the changed file, or "" if no decision was recorded.
func (e *Engine) SetAutoContinue(ctx context.Context, agentID, home string, enabled bool, setBy string, mode DefaultMode) (string, error) {
	if enabled {
		_, path, err := e.enable(ctx, agentID, home, 300, setBy, mode)
		return path, err
	}
	_, path, err := e.disable(agentID, home, setBy, mode)
	return path, err
}

func (e *Engine) enable(ctx context.Context, agentID, home string, poll int, setBy string, mode DefaultMode) (string, string, error) {
	agentID, err := e.validate(agentID, poll)
	if err != nil {
		return "", "", err
	}
	home = e.ResolveHome(home)
	path, err := e.registrationPath(agentID, home)
	if err != nil {
		return "", "", err
	}
	snapshot, err := e.Paseo.Inspect(ctx, agentID, home)
	if err != nil {
		return "", "", err
	}
	if !equal(snapshot.Value("Id"), agentID) || truthy(snapshot.Value("Archived")) {
		return "", "", refuse("Agent is unavailable or archived")
	}
	binding, err := e.binding(snapshot, home)
	if err != nil {
		return "", "", err
	}
	if _, err := e.stored(agentID, home); err != nil { // a readable local record first
		return "", "", err
	}
	unlock, err := lockFile(lockPath(path))
	if err != nil {
		return "", "", err
	}
	defer unlock()
	previous, err := readObject(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if setBy == "default" && !defaultMay(previous, true, mode) {
		return "", "", nil // A decision may have arrived since the listing.
	}
	if setBy != "cli" {
		if n, ok := numberOf(previous.Value("poll")); ok && n >= 30 {
			poll = int(n)
		}
	}
	state := Obj("version", 1, "enabled", true, "home", home, "agent_id", agentID,
		"binding", binding, "poll", poll, "status", StatusWaitingFailure,
		"registered_at", float64(e.now().UnixNano())/1e9, "set_by", setBy)
	if cache, ok := previous.Get("cache"); ok {
		state.Set("cache", cache)
	}
	if truthy(previous.Value("enabled")) && equal(previous.Value("binding"), binding) {
		state.Set("claimed_failure", previous.Value("claimed_failure"))
		if status, ok := previous.Get("status"); ok {
			state.Set("status", status)
		}
	}
	if err := atomicWrite(path, state); err != nil {
		return "", "", err
	}
	out := "auto-continue on: " + pyStr(binding.Value("agent")) + "/" + pyStr(binding.Value("profile")) +
		", target: paseo:" + agentID + ", poll: " + strconv.Itoa(poll) + "s"
	return out, path, nil
}

func (e *Engine) disable(agentID, home, setBy string, mode DefaultMode) (string, string, error) {
	agentID, err := e.validate(agentID, 300)
	if err != nil {
		return "", "", err
	}
	home = e.ResolveHome(home)
	path, err := e.registrationPath(agentID, home)
	if err != nil {
		return "", "", err
	}
	unlock, err := lockFile(lockPath(path))
	if err != nil {
		return "", "", err
	}
	defer unlock()
	state, err := readObject(path)
	if setBy == "default" && (errors.Is(err, os.ErrNotExist) || err == nil && !defaultMay(state, false, mode)) {
		return "", "", nil // nothing on to turn off, or a decision that stands
	}
	if errors.Is(err, os.ErrNotExist) {
		record, err := e.stored(agentID, home)
		if err != nil || !equal(record.Value("id"), agentID) {
			return "auto-continue was not on for this Paseo agent", "", nil
		}
		state = Obj("version", 1, "home", home, "agent_id", agentID, "poll", 300,
			"registered_at", float64(e.now().UnixNano())/1e9)
	} else if err != nil {
		return "", "", err
	}
	state.Set("enabled", false)
	state.Set("status", StatusOff)
	state.Set("set_by", setBy)
	if err := atomicWrite(path, state); err != nil {
		return "", "", err
	}
	return "auto-continue off for paseo:" + agentID, path, nil
}

// Status describes one registration in the old tool's words; the watcher
// line names the daemon instead of a tmux worker.
func (e *Engine) Status(ctx context.Context, agentID, home string) (string, error) {
	agentID, err := e.validate(agentID, 300)
	if err != nil {
		return "", err
	}
	path, err := e.registrationPath(agentID, e.ResolveHome(home))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "this Paseo agent: off", nil
	}
	state, err := readObject(path)
	if err != nil {
		return "", err
	}
	binding, _ := state.Object("binding")
	if binding == nil && !truthy(state.Value("enabled")) {
		return "this Paseo agent: off", nil
	}
	onOff := "off"
	if truthy(state.Value("enabled")) {
		onOff = "on"
	}
	lines := []string{
		"this Paseo agent: " + onOff + ", " + pyStr(state.Value("status")),
		"profile: " + pyStr(binding.Value("agent")) + "/" + pyStr(binding.Value("profile")),
		"target: paseo:" + agentID + ", poll: " + pyStr(state.Value("poll")) + "s",
		"watcher: agent-quotas daemon",
	}
	return strings.Join(lines, "\n"), nil
}

// Registration is one registration file, for the daemon's scheduler.
type Registration struct {
	Path       string `json:"path"`
	AgentID    string `json:"agent_id"`
	Home       string `json:"home"`
	Enabled    bool   `json:"enabled"`
	Poll       int    `json:"poll"` // seconds; 300 when unreadable
	Status     string `json:"status"`
	Agent      string `json:"agent"`   // binding: claude or codex
	Profile    string `json:"profile"` // binding: Claude profile, or "codex"
	SetBy      string `json:"set_by,omitempty"`
	NativeHome string `json:"native_home"`
	Provider   string `json:"provider"`
}

// Registrations lists every readable registration file, enabled or not,
// sorted by path. Unreadable files are skipped.
func (e *Engine) Registrations() ([]Registration, error) {
	paths, err := filepath.Glob(filepath.Join(e.StateDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []Registration
	for _, path := range paths {
		state, err := readObject(path)
		if err != nil {
			continue
		}
		binding, _ := state.Object("binding")
		r := Registration{Path: path, Enabled: truthy(state.Value("enabled")), Poll: 300}
		r.AgentID, _ = state.String("agent_id")
		r.Home, _ = state.String("home")
		r.Status, _ = state.String("status")
		r.SetBy, _ = state.String("set_by")
		r.Agent, _ = binding.String("agent")
		r.Profile, _ = binding.String("profile")
		r.NativeHome, _ = binding.String("native_home")
		r.Provider, _ = binding.String("provider")
		if n, ok := state.Value("poll").(json.Number); ok {
			if f, err := n.Float64(); err == nil && f >= 1 {
				r.Poll = int(f)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// pySplitJoin is " ".join(s.split()) with Python's whitespace set.
func pySplitJoin(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
	}), " ")
}

// List is the old `list` action: one tab-separated row per enabled
// registration (agent id, home, agent/profile, status).
func (e *Engine) List() ([]string, error) {
	dir := e.StateDir()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var rows []string
	for _, path := range paths {
		state, err := readObject(path)
		if err != nil || !truthy(state.Value("enabled")) {
			continue
		}
		binding, _ := state.Object("binding")
		field := func(o *Object, key, def string) string {
			v, ok := o.Get(key)
			if !ok {
				return def
			}
			return pyStr(v)
		}
		row := []string{field(state, "agent_id", ""), field(state, "home", ""),
			field(binding, "agent", "?") + "/" + field(binding, "profile", "?"), field(state, "status", "")}
		if !truthy(state.Value("agent_id")) || !truthy(state.Value("home")) {
			continue
		}
		for i := range row {
			row[i] = pySplitJoin(row[i])
		}
		rows = append(rows, strings.Join(row, "\t"))
	}
	return rows, nil
}

// stoppedAgents ports stopped_agents: the local agents whose last turn
// stopped at a usage limit, registered or not, from the stored records alone.
func (e *Engine) stoppedAgents(home string) ([]string, error) {
	agents := filepath.Join(home, "agents")
	m1, _ := filepath.Glob(filepath.Join(agents, "*.json"))
	m2, _ := filepath.Glob(filepath.Join(agents, "*", "*.json"))
	paths := append(m1, m2...)
	sort.Strings(paths)
	type cached struct {
		id Identity
		ok bool
	}
	identities := map[string]cached{}
	var out []string
	for _, path := range paths {
		record, err := readObject(path)
		if err != nil {
			continue // unreadable, not JSON, or not an object
		}
		if truthy(record.Value("archivedAt")) || truthy(record.Value("internal")) {
			continue
		}
		agentID, ok := record.String("id")
		if !ok {
			continue
		}
		status, _ := record.String("lastStatus")
		if (status == "error" || status == "closed") && quotaErrorOf(record) {
			out = append(out, agentID)
			continue
		}
		if status != "idle" && status != "closed" {
			continue
		}
		provider := record.Value("provider")
		key := pyStr(provider)
		if _, isStr := provider.(string); !isStr {
			key = "\x00" + key
		}
		c, seen := identities[key]
		if !seen {
			id, err := e.providerConfig(home, provider)
			c = cached{id: id, ok: err == nil}
			identities[key] = c
		}
		if !c.ok || (c.id.Agent != "claude" && c.id.Agent != "codex") {
			continue
		}
		failure, err := e.transcript(record, c.id)
		if err != nil {
			return nil, err
		}
		if failure != "" {
			out = append(out, agentID)
		}
	}
	return out, nil
}

func (e *Engine) checkable(home string, provider any) bool {
	if e.checkableFn != nil {
		return e.checkableFn(provider)
	}
	_, err := e.providerConfig(home, provider)
	return err == nil
}

func (e *Engine) agyAccountOf(home string, provider any) (string, bool, error) {
	if e.agyAccountFn != nil {
		return e.agyAccountFn(provider)
	}
	return agyAccount(home, provider)
}

// usableAgents ports usable_agents: the agents whose seat has quota left for
// their model now. One check per (provider, model family) and per
// (Antigravity account, model). Unknown Antigravity quota and providers whose
// quota is not read here keep their agents, with a note; a failed Claude or
// Codex check leaves its agents out, with a note.
func (e *Engine) usableAgents(ctx context.Context, home string, agentIDs []string, warn func(string)) ([]string, error) {
	type key struct{ kind, a, b string }
	verdicts := map[key]bool{}
	var out []string
	for _, agentID := range agentIDs {
		record, err := e.stored(agentID, home)
		if err != nil {
			warn(agentID + ": no readable record; left out")
			continue
		}
		provider := record.Value("provider")
		model := ""
		if cfg, ok := record.Value("config").(*Object); ok && truthy(cfg.Value("model")) {
			model = pyStr(cfg.Value("model"))
		} else if rt, ok := record.Value("runtimeInfo").(*Object); ok && truthy(rt.Value("model")) {
			model = pyStr(rt.Value("model"))
		}
		checkable := e.checkable(home, provider)
		if !checkable {
			account, isAgy, err := e.agyAccountOf(home, provider)
			if err != nil {
				return out, err
			}
			if isAgy {
				k := key{"agy", account, model}
				if _, done := verdicts[k]; !done {
					usable, err := e.agyQuota(ctx, account, model)
					if err != nil {
						warn("Antigravity " + account + " " + model + ": quota unknown (" + err.Error() + "); its agents are kept")
						usable = true
					}
					verdicts[k] = usable
				}
				if verdicts[k] {
					out = append(out, agentID)
				}
				continue
			}
			warn(agentID + ": " + pyStr(provider) + " quota is not checked here; kept")
			out = append(out, agentID)
			continue
		}
		k := key{"seat", pyStr(provider), ModelFamily(model)}
		if _, done := verdicts[k]; !done {
			usable, err := e.quotaFor(ctx, home, provider, model)
			if err != nil {
				warn(pyStr(provider) + ": quota check failed (" + err.Error() + "); its agents are left out")
				usable = false
			}
			verdicts[k] = usable
		}
		if verdicts[k] {
			out = append(out, agentID)
		}
	}
	return out, nil
}

func (e *Engine) agyQuota(ctx context.Context, account, model string) (bool, error) {
	if e.Quotas == nil {
		return false, errors.New("no quota source")
	}
	return e.Quotas.Agy(ctx, account, model)
}

// Stopped is the old `stopped` action: the local agents of home ("" means
// the default home) whose last turn stopped at a usage limit, subagents
// included, registered or not. With usable, only those whose seat has quota
// left for their model now; warn receives the notes the old tool printed on
// stderr (nil discards them).
func (e *Engine) Stopped(ctx context.Context, home string, usable bool, warn func(string)) ([]string, error) {
	home = e.ResolveHome(home)
	found, err := e.stoppedAgents(home)
	if err != nil || !usable {
		return found, err
	}
	if warn == nil {
		warn = func(string) {}
	}
	return e.usableAgents(ctx, home, found, warn)
}
