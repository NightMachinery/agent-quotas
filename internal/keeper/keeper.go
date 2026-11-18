// Package keeper is the cache policy for idle Paseo agents: keepalive pings
// and compaction before the provider's prompt cache expires. It shares the
// Paseo auto-continue registration files and their locks. See
// docs/design.md, "Keeper".
package keeper

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// Policies.
const (
	PolicyOff     = "off"
	PolicyCompact = "compact"
	PolicyWarm    = "warm"
)

// Phases, derived from the policy and the time (never stored).
const (
	PhaseOff     = "off"
	PhaseCompact = "compact"
	PhaseStandby = "standby"
	PhaseWarming = "warming"
	PhaseDormant = "dormant"
)

// Compaction states.
const (
	StateQueued    = "queued"
	StateSent      = "sent"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// Action kinds.
const (
	ActNone    = "none"
	ActPing    = "ping"
	ActCompact = "compact"
)

// Activity sources.
const (
	SourceTranscript = "transcript"
	SourcePaseo      = "paseo"
)

// overageTTL is Claude's prompt-cache lifetime on extra usage.
const overageTTL = 300

// ownSendSlack is how close Paseo's lastUserMessageAt must be to a keeper
// send for the message to be that send.
const ownSendSlack = 15.0

// defaultMinContext is min_context_tokens when ~/.compaction.yaml says
// nothing.
const defaultMinContext = 128000

// Errors the API maps to statuses.
var (
	ErrDisabled     = errors.New("the keeper is disabled (keeper.enabled is false)")
	ErrUnknownAgent = errors.New("Paseo does not know this agent")
	ErrBadRequest   = errors.New("bad request")
)

// Paseo is the access to the Paseo daemon the keeper needs.
type Paseo interface {
	Inspect(ctx context.Context, agentID, home string) (*paseo.Object, error)
	Send(ctx context.Context, agentID, home, text, behavior string) error
	Logs(ctx context.Context, agentID, home string, since time.Time) ([]*paseo.Object, error)
	List(ctx context.Context, home string) ([]*paseo.Object, error)
}

// Seat is the quota seat an agent runs on.
type Seat struct {
	Account string `json:"account"`
	Verdict string `json:"verdict"`
	Overage bool   `json:"overage"`
}

// Seats resolves an agent's identity to its seat, from the daemon's store.
type Seats interface {
	Seat(ctx context.Context, id paseo.Identity) (Seat, error)
}

// Ping is the last keepalive ping.
type Ping struct {
	At    float64 `json:"at"`
	OK    bool    `json:"ok"`
	Error string  `json:"error"`
}

// Pings counts keeper pings since the real activity Since.
type Pings struct {
	Since float64 `json:"since"`
	Count int     `json:"count"`
}

// Compaction is the latest compaction request.
type Compaction struct {
	ID           string  `json:"id"`
	Reason       string  `json:"reason"` // policy | standby | manual
	State        string  `json:"state"`
	RequestedAt  float64 `json:"requested_at"`
	SentAt       float64 `json:"sent_at"`
	FinishedAt   float64 `json:"finished_at"`
	TokensBefore *int64  `json:"tokens_before"`
	TokensAfter  *int64  `json:"tokens_after"`
	Error        string  `json:"error"`
}

// Terminal reports whether the compaction has ended.
func (c *Compaction) Terminal() bool {
	return c != nil && (c.State == StateCompleted || c.State == StateFailed || c.State == StateCanceled)
}

func (c *Compaction) inFlight() bool {
	return c != nil && (c.State == StateQueued || c.State == StateSent)
}

// Cache is the registration file's `cache` object.
type Cache struct {
	Policy     string      `json:"policy"`
	WarmHours  float64     `json:"warm_hours"`
	SetAt      float64     `json:"set_at"`
	SetBy      string      `json:"set_by"`
	StandbyAt  *float64    `json:"standby_at"`
	ActivityAt float64     `json:"activity_at"`
	Pings      Pings       `json:"pings"`
	LastPing   *Ping       `json:"last_ping"`
	Compaction *Compaction `json:"compaction"`
}

// Keeper runs cache policies. Use New.
type Keeper struct {
	Config *config.Config
	Engine *paseo.Engine
	Paseo  Paseo
	Seats  Seats
	Log    *log.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// NewID makes compaction ids; nil means random "c-" + 12 hex digits.
	NewID func() string

	mu       sync.Mutex
	changed  chan struct{}
	busy     map[string]time.Time // registration path -> lock found held
	lastSeen map[string]string    // registration path -> last logged state
	kick     chan string
}

// New returns a keeper over the Paseo engine's registrations.
func New(cfg *config.Config, eng *paseo.Engine, p Paseo, seats Seats, logger *log.Logger) *Keeper {
	if logger == nil {
		logger = log.New(os.Stderr, "keeper: ", log.LstdFlags)
	}
	return &Keeper{Config: cfg, Engine: eng, Paseo: p, Seats: seats, Log: logger}
}

func (k *Keeper) init() {
	if k.changed == nil {
		k.changed = make(chan struct{})
		k.busy = map[string]time.Time{}
		k.lastSeen = map[string]string{}
		k.kick = make(chan string, 16)
	}
}

func (k *Keeper) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Keeper) nowS() float64 { return model.Unix(k.now()) }

// Enabled reports whether the keeper may observe and send.
func (k *Keeper) Enabled() bool { return k.Config.KeeperEnabled() }

func (k *Keeper) newID() string {
	if k.NewID != nil {
		return k.NewID()
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "c-" + hex.EncodeToString(b[:])
}

// signal wakes long-polls after a state write.
func (k *Keeper) signal() {
	k.mu.Lock()
	k.init()
	close(k.changed)
	k.changed = make(chan struct{})
	k.mu.Unlock()
}

func (k *Keeper) changes() <-chan struct{} {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.init()
	return k.changed
}

// PingText is the configured ping, one line, starting with the marker.
func (k *Keeper) PingText() string { return PingText(k.Config.Keeper.PingText) }

// PingText normalizes a ping text: one line, the marker prefixed when
// missing.
func PingText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		text = config.DefaultPingText
	}
	if !strings.HasPrefix(text, paseo.KeeperMarker) {
		text = paseo.KeeperMarker + " " + text
	}
	return text
}

// MinContextTokens reads min_context_tokens from the compaction file
// (a flat YAML file; only that key is read).
func MinContextTokens(path string) int64 {
	f, err := os.Open(config.Expand(path))
	if err != nil {
		return defaultMinContext
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "min_context_tokens" || strings.HasPrefix(key, " ") {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		val = strings.ReplaceAll(val, "_", "")
		if n, err := strconv.ParseInt(val, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return defaultMinContext
}

// ---- reading and writing the cache object ----

func readCache(state *paseo.Object) (*Cache, error) {
	o, ok := state.Object("cache")
	if !ok {
		return nil, nil
	}
	text, err := paseo.Dumps(o, false)
	if err != nil {
		return nil, err
	}
	var c Cache
	if err := json.Unmarshal([]byte(text), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func writeCache(state *paseo.Object, c *Cache) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	o, err := paseo.DecodeObject(data)
	if err != nil {
		return err
	}
	state.Set("cache", o)
	return nil
}

// ---- observation ----

// obs is one look at an agent: its file, Paseo's record, the transcript and
// the seat.
type obs struct {
	path    string
	exists  bool
	state   *paseo.Object
	cache   *Cache
	agentID string
	home    string
	record  *paseo.Object
	inspect *paseo.Object // nil when not inspected
	ident   paseo.Identity
	identOK bool
	act     paseo.Activity
	seat    *Seat
	failure string
}

// observe reads everything about one agent. With inspect, `paseo inspect`
// is asked too; otherwise Paseo's stored record stands in for it.
func (k *Keeper) observe(ctx context.Context, agentID, home string, inspect bool) (*obs, error) {
	home = k.Engine.ResolveHome(home)
	path, err := k.Engine.RegistrationPath(agentID, home)
	if err != nil {
		return nil, err
	}
	o := &obs{path: path, agentID: agentID, home: home}
	if state, err := paseo.ReadObject(path); err == nil {
		o.exists, o.state = true, state
		if h, ok := state.String("home"); ok && h != "" {
			o.home = h
		}
		if o.cache, err = readCache(state); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	o.record, _ = k.Engine.Stored(agentID, o.home)
	if inspect {
		o.inspect, err = k.Paseo.Inspect(ctx, agentID, o.home)
		if err != nil {
			return nil, err
		}
	}
	if o.record == nil && o.inspect == nil && !o.exists {
		return nil, ErrUnknownAgent
	}
	// Identity: the file's binding, else the provider Paseo reports.
	if b, ok := o.state.Object("binding"); ok {
		o.ident, o.identOK = paseo.IdentityOf(b), true
	} else {
		snap := o.inspect
		if snap == nil && o.record != nil {
			snap = paseo.Obj("Provider", o.record.Value("provider"), "Cwd", o.record.Value("cwd"))
		}
		if snap != nil {
			if b, err := k.Engine.Binding(snap, o.home); err == nil {
				o.ident, o.identOK = paseo.IdentityOf(b), true
			}
		}
	}
	if o.identOK && o.record != nil {
		o.act, _ = k.Engine.Activity(o.record, o.ident)
	}
	if o.identOK && k.Seats != nil {
		if s, err := k.Seats.Seat(ctx, o.ident); err == nil {
			o.seat = &s
		}
	}
	if o.identOK && o.record != nil {
		snap := o.inspect
		if snap == nil {
			snap = paseo.Obj("Status", o.record.Value("lastStatus"))
		}
		b, _ := o.state.Object("binding")
		if b == nil {
			b = paseo.Obj("agent", o.ident.Agent, "profile", o.ident.Profile, "native_home", o.ident.NativeHome)
		}
		o.failure, _ = k.Engine.Failure(snap, o.record, b)
	}
	return o, nil
}

func (o *obs) status() string {
	if o.inspect != nil {
		s, _ := o.inspect.String("Status")
		return s
	}
	s, _ := o.record.String("lastStatus")
	return s
}

func (o *obs) archived() bool {
	if o.inspect != nil {
		if v, _ := o.inspect.Value("Archived").(bool); v {
			return true
		}
	}
	return o.record != nil && o.record.Value("archivedAt") != nil
}

func (o *obs) pendingPermission() bool {
	if o.inspect != nil {
		list, _ := o.inspect.Value("PendingPermissions").([]any)
		return len(list) > 0
	}
	reason, _ := o.record.String("attentionReason")
	need, _ := o.record.Value("requiresAttention").(bool)
	return need && reason == "permission"
}

func (o *obs) title() *string {
	if o.inspect != nil {
		if s, ok := o.inspect.String("Name"); ok && s != "" {
			return &s
		}
	}
	if s, ok := o.record.String("title"); ok && s != "" {
		return &s
	}
	return nil
}

func (o *obs) claimed() string {
	s, _ := o.state.String("claimed_failure")
	return s
}

// ownSend reports whether a Paseo user-message time is one of the keeper's
// own sends.
func (o *obs) ownSend(at float64) bool {
	if at <= 0 || o.cache == nil {
		return false
	}
	near := func(t float64) bool { return t > 0 && math.Abs(at-t) <= ownSendSlack }
	if o.cache.LastPing != nil && near(o.cache.LastPing.At) {
		return true
	}
	return o.cache.Compaction != nil && near(o.cache.Compaction.SentAt)
}

// ---- the derived view ----

// Action is the next thing the keeper will do for an agent.
type Action struct {
	Kind   string   `json:"kind"`
	At     *float64 `json:"at"`
	Reason string   `json:"reason"`
}

// Sendable says whether the keeper may send to the agent now.
type Sendable struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

// AutoContinue is the agent's auto-continue registration.
type AutoContinue struct {
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
}

// CacheView is the stored cache object plus its phase.
type CacheView struct {
	Cache
	Phase string `json:"phase"`
}

// View is the API's Agent document.
type View struct {
	ID                 string        `json:"id"`
	Home               string        `json:"home"`
	Title              *string       `json:"title"`
	Provider           string        `json:"provider"`
	Status             string        `json:"status"`
	PendingPermission  bool          `json:"pending_permission"`
	Archived           bool          `json:"archived"`
	LastActivityAt     *float64      `json:"last_activity_at"`
	LastActivitySource string        `json:"last_activity_source"`
	CacheActivityAt    *float64      `json:"cache_activity_at"`
	ContextTokens      *int64        `json:"context_tokens"`
	MinContextTokens   int64         `json:"min_context_tokens"`
	Big                bool          `json:"big"`
	TTLSeconds         int           `json:"ttl_seconds"`
	TTLReason          string        `json:"ttl_reason"`
	CacheExpiresAt     *float64      `json:"cache_expires_at"`
	Seat               *Seat         `json:"seat"`
	Cache              *CacheView    `json:"cache"`
	WarmUntil          *float64      `json:"warm_until"`
	PingsSinceActivity int           `json:"pings_since_activity"`
	Sendable           Sendable      `json:"sendable"`
	NextAction         Action        `json:"next_action"`
	AutoContinue       *AutoContinue `json:"auto_continue"`
	LockPath           *string       `json:"lock_path"`

	// realAt etc. are the plain numbers behind the pointers.
	realAt, cacheAt, expiry, warmUntil float64
	compacted                          bool
}

func fptr(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

// view derives everything the API and the scheduler need from one
// observation.
func (k *Keeper) view(o *obs) View {
	cfg := k.Config.Keeper
	now := k.nowS()
	v := View{ID: o.agentID, Home: o.home, Title: o.title(), Status: o.status(),
		PendingPermission: o.pendingPermission(), Archived: o.archived(), Seat: o.seat,
		MinContextTokens: MinContextTokens(cfg.CompactionFile)}
	if o.identOK {
		v.Provider = o.ident.Agent
	} else if o.record != nil {
		v.Provider, _ = o.record.String("provider")
	}
	if o.exists {
		lp := paseo.LockPath(o.path)
		v.LockPath = &lp
		ac := AutoContinue{}
		ac.Enabled, _ = o.state.Value("enabled").(bool)
		ac.Status, _ = o.state.String("status")
		v.AutoContinue = &ac
	}

	// Real activity.
	c := o.cache
	if o.act.Found && o.act.RealAt > 0 {
		v.realAt, v.LastActivitySource = o.act.RealAt, SourceTranscript
	} else {
		v.LastActivitySource = SourcePaseo
		la := paseo.ParseTime(o.record.Value("lastActivityAt"))
		lu := paseo.ParseTime(o.record.Value("lastUserMessageAt"))
		if la > 0 && !o.ownSend(lu) {
			v.realAt = la
		}
		if c != nil && c.ActivityAt > v.realAt {
			v.realAt = c.ActivityAt
		}
	}
	// Cache activity.
	v.cacheAt = math.Max(v.realAt, o.act.CacheAt)
	if !o.act.Found {
		v.cacheAt = math.Max(v.cacheAt, paseo.ParseTime(o.record.Value("lastActivityAt")))
	}
	if c != nil {
		if c.LastPing != nil && c.LastPing.OK {
			v.cacheAt = math.Max(v.cacheAt, c.LastPing.At)
		}
		if c.Compaction != nil && c.Compaction.State == StateCompleted {
			v.cacheAt = math.Max(v.cacheAt, c.Compaction.FinishedAt)
		}
	}
	v.LastActivityAt, v.CacheActivityAt = fptr(v.realAt), fptr(v.cacheAt)

	if o.act.HasContext {
		n := o.act.ContextTokens
		v.ContextTokens = &n
		v.Big = n >= v.MinContextTokens
	}
	v.TTLSeconds, v.TTLReason = cfg.TTLSeconds[v.Provider], "config"
	if v.TTLSeconds <= 0 {
		v.TTLSeconds = 3600
	}
	if o.seat != nil && o.seat.Overage {
		v.TTLSeconds, v.TTLReason = overageTTL, "overage"
	}
	if v.cacheAt > 0 {
		v.expiry = v.cacheAt + float64(v.TTLSeconds)
		v.CacheExpiresAt = &v.expiry
	}

	if c != nil {
		cv := CacheView{Cache: *c, Phase: PhaseOff}
		hours := c.WarmHours
		if hours <= 0 {
			hours = cfg.WarmHours
		}
		switch c.Policy {
		case PolicyCompact:
			cv.Phase = PhaseCompact
			if c.StandbyAt != nil {
				cv.Phase = PhaseStandby
			}
		case PolicyWarm:
			v.warmUntil = math.Max(v.realAt, c.SetAt) + hours*3600
			v.WarmUntil = &v.warmUntil
			cv.Phase = PhaseWarming
			if now >= v.warmUntil {
				cv.Phase = PhaseDormant
			}
		}
		v.Cache = &cv
		if c.Pings.Count > 0 && math.Abs(c.Pings.Since-v.realAt) < 0.001 {
			v.PingsSinceActivity = c.Pings.Count
		}
		comp := c.Compaction
		v.compacted = comp != nil && comp.State == StateCompleted && comp.RequestedAt >= v.realAt
	}
	if o.act.CompactedAt > 0 && o.act.CompactedAt >= v.realAt {
		v.compacted = true
	}
	v.NextAction = k.plan(&v, now)
	kind := v.NextAction.Kind
	if kind == ActNone {
		kind = ActPing
	}
	v.Sendable = k.sendable(o, kind, now)
	return v
}

// plan is the scheduling rule of the design, for the time now.
func (k *Keeper) plan(v *View, now float64) Action {
	none := func(reason string) Action { return Action{Kind: ActNone, Reason: reason} }
	at := func(kind string, t float64, reason string) Action { return Action{Kind: kind, At: &t, Reason: reason} }
	if v.Archived {
		return none("archived")
	}
	if v.Cache == nil {
		return none("no cache policy")
	}
	c := v.Cache
	if comp := c.Compaction; comp != nil {
		switch comp.State {
		case StateQueued:
			return at(ActCompact, math.Max(comp.RequestedAt, 0), comp.Reason+" compaction queued")
		case StateSent:
			return none("compaction " + comp.ID + " in progress")
		}
	}
	if c.Policy == PolicyOff || c.Policy == "" {
		return none("policy off")
	}
	if v.expiry <= 0 {
		return none("no activity known")
	}
	due := v.expiry - float64(k.Config.Keeper.LeadSeconds)
	if now >= v.expiry {
		return none("cache expired; window missed, waiting for a real turn")
	}
	warm := c.Phase == PhaseWarming && due < v.warmUntil
	if v.Big && !v.compacted {
		reason := "policy"
		if c.Phase == PhaseStandby {
			reason = "standby"
		}
		return at(ActCompact, due, reason+": big context before cache expiry")
	}
	if warm {
		return at(ActPing, due, "keep the cache warm")
	}
	switch {
	case c.Phase == PhaseWarming:
		return none("the warm ceiling comes before the next ping")
	case v.compacted:
		return none("compacted in this idle stretch")
	case c.Phase == PhaseDormant:
		return none("dormant (past the warm ceiling); context below the compaction minimum")
	}
	return none("context below the compaction minimum; the cache may go cold")
}

// sendable is the gate every send passes, for an action kind.
func (k *Keeper) sendable(o *obs, kind string, now float64) Sendable {
	no := func(r string) Sendable { return Sendable{Reason: r} }
	if o.archived() {
		return no("archived")
	}
	switch st := o.status(); st {
	case "idle":
	case "closed":
		if kind == ActPing {
			return no("closed: Paseo unloaded it")
		}
	case "":
		return no("status unknown")
	default:
		return no(st)
	}
	if o.pendingPermission() {
		return no("pending permission")
	}
	if o.seat != nil && o.seat.Verdict == model.Blocked {
		return no("seat " + o.seat.Account + " blocked")
	}
	if o.failure != "" && o.failure != o.claimed() {
		return no("unclaimed quota-failed turn (auto-continue's)")
	}
	quiet := float64(k.Config.Keeper.QuietSeconds)
	k.mu.Lock()
	busy, held := k.busy[o.path]
	k.mu.Unlock()
	if held && now-model.Unix(busy) < quiet {
		return no("lock held by another sender recently")
	}
	if lu := paseo.ParseTime(o.record.Value("lastUserMessageAt")); lu > 0 && now-lu < quiet && !o.ownSend(lu) {
		return no("a message was sent recently")
	}
	return Sendable{OK: true}
}
